// Package datadog turns Datadog security findings into attention items. It
// reads the findings endpoint with plain net/http and never writes to Datadog:
// attentiond senses, and whatever triages the queue does so elsewhere.
package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// SourceName is the item source this adapter owns.
const SourceName = "datadog"

// DefaultSite is Datadog's EU1 site. The API host is api.<site>.
const DefaultSite = "datadoghq.eu"

// findingsPath is the findings search endpoint.
const findingsPath = "/api/v2/security/findings"

// maxResponse bounds one page. A vulnerability finding carries cloud
// configuration, risk evidence and workflow history, so a page of a thousand
// is tens of megabytes before it is a problem.
const maxResponse = 64 << 20

// BaseURL returns the API origin for a site, for example
// https://api.datadoghq.eu.
func BaseURL(site string) string {
	return "https://api." + strings.TrimSpace(site)
}

// Client talks to one Datadog site with one pair of keys.
type Client struct {
	base   string
	apiKey string
	appKey string
	http   *http.Client
}

// NewClient returns a client for the API origin base. The keys are sent as
// headers, never logged, and scrubbed from every error this package returns.
func NewClient(base, apiKey, appKey string, timeout time.Duration) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		apiKey: apiKey,
		appKey: appKey,
		http:   &http.Client{Timeout: timeout},
	}
}

// ResolveKeys reads the API key and the application key from the environment
// variables the config names. It reports a missing one by variable name, never
// by value.
func ResolveKeys(apiKeyEnv, appKeyEnv string) (apiKey, appKey string, err error) {
	apiKey = strings.TrimSpace(os.Getenv(apiKeyEnv))
	appKey = strings.TrimSpace(os.Getenv(appKeyEnv))

	var missing []string
	if apiKey == "" {
		missing = append(missing, apiKeyEnv)
	}
	if appKey == "" {
		missing = append(missing, appKeyEnv)
	}
	if len(missing) > 0 {
		return "", "", fmt.Errorf("%s not set", strings.Join(missing, " and "))
	}
	return apiKey, appKey, nil
}

// Response is one page of the findings endpoint.
type Response struct {
	Data []Record `json:"data"`
	Meta Meta     `json:"meta"`
}

// Meta carries the cursor and whether Datadog finished the search.
type Meta struct {
	Page struct {
		After string `json:"after"`
	} `json:"page"`
	// Status is "done" or "timeout". A timeout means the page is a prefix
	// of what matched.
	Status string `json:"status"`
}

// Record is one entry of data[]. Datadog nests the finding one level deeper
// than the envelope suggests: attributes.attributes.
type Record struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Attributes Finding `json:"attributes"`
	} `json:"attributes"`
}

// Finding is the slice of a host_and_container_vulnerability finding this
// adapter reads.
type Finding struct {
	FindingID   string `json:"finding_id"`
	FindingType string `json:"finding_type"`
	Title       string `json:"title"`
	Severity    string `json:"severity"`
	Status      string `json:"status"`
	// FirstSeenAt is epoch milliseconds, zero when absent.
	FirstSeenAt    int64          `json:"first_seen_at"`
	ContainerImage ContainerImage `json:"container_image"`
	Package        Package        `json:"package"`
	Remediation    Remediation    `json:"remediation"`
	Vulnerability  Vulnerability  `json:"vulnerability"`

	// recordID is the envelope id, kept for findings that omit finding_id.
	recordID string
}

// ContainerImage is the image the vulnerable package was found in.
type ContainerImage struct {
	Name        string   `json:"name"`
	RepoDigests []string `json:"repo_digests"`
}

// Package is the vulnerable package.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Remediation says whether a fix exists and which version carries it.
type Remediation struct {
	// IsAvailable is nil when Datadog did not say, which is different from
	// saying there is no fix.
	IsAvailable *bool              `json:"is_available"`
	Package     RemediationPackage `json:"package"`
}

// RemediationPackage lists the versions that fix the finding. Base is the fix
// for the package as installed; ClosestNoVulnerabilities is the nearest
// version with no known vulnerability at all.
type RemediationPackage struct {
	Base                     []PackageVersion `json:"base"`
	ClosestNoVulnerabilities []PackageVersion `json:"closest_no_vulnerabilities"`
}

// PackageVersion is one candidate version.
type PackageVersion struct {
	Version string `json:"version"`
}

// Vulnerability is what Datadog knows about the flaw itself.
type Vulnerability struct {
	// IsInheritedFromBaseImage is nil when Datadog did not say.
	IsInheritedFromBaseImage *bool `json:"is_inherited_from_base_image"`
	Stack                    struct {
		Ecosystem string `json:"ecosystem"`
	} `json:"stack"`
}

// ID is the finding id: the finding's own, else the envelope's.
func (f Finding) ID() string {
	if f.FindingID != "" {
		return f.FindingID
	}
	return f.recordID
}

// Decode parses one response body, from the API or from a fixture.
func Decode(raw []byte) (Response, error) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Response{}, fmt.Errorf("decode datadog response: %w", err)
	}
	return resp, nil
}

// Findings flattens a response's records into findings.
func (r Response) Findings() []Finding {
	out := make([]Finding, 0, len(r.Data))
	for _, record := range r.Data {
		finding := record.Attributes.Attributes
		finding.recordID = record.ID
		out = append(out, finding)
	}
	return out
}

// Result is everything one poll learned.
type Result struct {
	Findings []Finding
	// Partial reports that Datadog timed out searching, so the findings are
	// a prefix of what matched.
	Partial bool
}

// ReadFixture decodes a recorded response from disk. It returns the same
// Result a live poll would.
func ReadFixture(path string) (Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	resp, err := Decode(raw)
	if err != nil {
		return Result{}, err
	}
	return Result{Findings: resp.Findings(), Partial: resp.Meta.Status == "timeout"}, nil
}

// Findings runs the query and follows meta.page.after until it is absent.
func (c *Client) Findings(ctx context.Context, query string, pageLimit int) (Result, error) {
	var result Result
	seen := map[string]bool{}
	cursor := ""
	for {
		resp, err := c.page(ctx, query, pageLimit, cursor)
		if err != nil {
			return Result{}, err
		}
		result.Findings = append(result.Findings, resp.Findings()...)
		if resp.Meta.Status == "timeout" {
			result.Partial = true
		}

		cursor = resp.Meta.Page.After
		if cursor == "" {
			return result, nil
		}
		// A cursor that comes back unchanged would page forever.
		if seen[cursor] {
			return Result{}, fmt.Errorf("datadog findings: cursor repeated, stopping")
		}
		seen[cursor] = true
	}
}

func (c *Client) page(ctx context.Context, query string, limit int, cursor string) (Response, error) {
	params := url.Values{}
	if query != "" {
		params.Set("filter[query]", query)
	}
	if limit > 0 {
		params.Set("page[limit]", strconv.Itoa(limit))
	}
	if cursor != "" {
		params.Set("page[cursor]", cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+findingsPath+"?"+params.Encode(), nil)
	if err != nil {
		return Response{}, c.scrub(err)
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, c.scrub(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return Response{}, c.scrub(err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, c.scrub(fmt.Errorf("datadog findings: %s: %s", resp.Status, firstLine(raw)))
	}
	decoded, err := Decode(raw)
	if err != nil {
		return Response{}, c.scrub(err)
	}
	return decoded, nil
}

// scrub removes the keys from an error. Nothing here puts one in a message,
// but a proxy or a server that echoes headers into a body would, and an error
// ends up in logs and in /health.
func (c *Client) scrub(err error) error {
	text := err.Error()
	for _, key := range []string{c.apiKey, c.appKey} {
		if key != "" {
			text = strings.ReplaceAll(text, key, "[redacted]")
		}
	}
	return errors.New(text)
}

func firstLine(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}
