package hippocampus

import "strings"

// maxReportedVersionBytes bounds what the topology view keeps of a version reported by the far end
// - a caller's contract.ClientVersionHeader, or a declared component's /readyz body. A real one is a
// dozen or two bytes; the bound exists because the value arrives from somebody else.
const maxReportedVersionBytes = 64

// sanitiseReportedVersion returns a reported version fit to keep and display, or "" when there is
// nothing usable. Anything outside printable ASCII rejects the whole value rather than being
// stripped from it, since a value that needed editing is not the one the far end sent; an overlong
// value is rejected for the same reason rather than truncated into a different-looking version.
func sanitiseReportedVersion(raw string) string {
	value := strings.TrimSpace(raw)

	if value == "" || len(value) > maxReportedVersionBytes {
		return ""
	}

	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return ""
		}
	}

	return value
}
