package datadog

import (
	"strconv"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Label is the word every finding carries. Mourningstar reads it, so it is
// part of the contract rather than a display choice.
const Label = "vulnerability"

// Normalize turns findings into items. A finding with no id cannot be keyed
// and is skipped; one that appears twice, as it can when a result set shifts
// between pages, is reported once.
func Normalize(findings []Finding, now time.Time) []attention.Item {
	items := make([]attention.Item, 0, len(findings))
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		id := finding.ID()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		items = append(items, newItem(finding, id, now))
	}
	return items
}

func newItem(f Finding, id string, now time.Time) attention.Item {
	meta := map[string]string{"finding_id": id}
	putIfSet(meta, "finding_type", f.FindingType)
	putIfSet(meta, "severity", f.Severity)
	putIfSet(meta, "image", f.ContainerImage.Name)
	if len(f.ContainerImage.RepoDigests) > 0 {
		putIfSet(meta, "repo_digest", f.ContainerImage.RepoDigests[0])
	}
	putIfSet(meta, "package", f.Package.Name)
	putIfSet(meta, "package_version", f.Package.Version)
	if f.Remediation.IsAvailable != nil {
		meta["fix_available"] = strconv.FormatBool(*f.Remediation.IsAvailable)
	}
	putIfSet(meta, "fixed_version", fixedVersion(f.Remediation.Package))
	if f.Vulnerability.IsInheritedFromBaseImage != nil {
		meta["inherited_from_base_image"] = strconv.FormatBool(*f.Vulnerability.IsInheritedFromBaseImage)
	}
	putIfSet(meta, "ecosystem", f.Vulnerability.Stack.Ecosystem)
	putIfSet(meta, "status", f.Status)

	updated := now
	if f.FirstSeenAt > 0 {
		first := time.UnixMilli(f.FirstSeenAt).UTC()
		meta["first_seen_at"] = first.Format(time.RFC3339)
		// How long a vulnerability has been open is the useful age.
		updated = first
	}

	return attention.Item{
		ID:        attention.Key(SourceName, id),
		Source:    SourceName,
		Title:     f.Title,
		State:     attention.StateNeedsAttention,
		Severity:  severity(f.Severity),
		Label:     Label,
		Tone:      attention.ToneAttention,
		Priority:  attention.DefaultPriority(attention.StateNeedsAttention),
		Context:   meta,
		UpdatedAt: updated,
	}
}

// fixedVersion prefers the fix for the package as installed, and falls back
// to the nearest version with no known vulnerabilities.
func fixedVersion(p RemediationPackage) string {
	if len(p.Base) > 0 && p.Base[0].Version != "" {
		return p.Base[0].Version
	}
	if len(p.ClosestNoVulnerabilities) > 0 {
		return p.ClosestNoVulnerabilities[0].Version
	}
	return ""
}

// severity maps Datadog's scale onto the three the queue has. Critical is the
// loudest the queue goes, so critical and high share it.
func severity(datadog string) attention.Severity {
	switch strings.ToLower(datadog) {
	case "critical", "high":
		return attention.SeverityCritical
	case "medium":
		return attention.SeverityWarning
	default:
		return attention.SeverityInfo
	}
}

func putIfSet(meta map[string]string, key, value string) {
	if value != "" {
		meta[key] = value
	}
}
