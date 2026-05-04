package pxc

import "testing"

// TestValidGTIDLiteral_AcceptsRealisticForms covers the canonical GTID forms
// MySQL emits: single-transaction `<UUID>:N`, ranges `<UUID>:1-100`, and
// multi-source intervals separated by ':'.
func TestValidGTIDLiteral_AcceptsRealisticForms(t *testing.T) {
	cases := []string{
		"3E11FA47-71CA-11E1-9E33-C80AA9429562:1",
		"3e11fa47-71ca-11e1-9e33-c80aa9429562:1-100",
		"3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100:200-300",
		"abcdef0123456789",
	}
	for _, c := range cases {
		if !validGTIDLiteral(c) {
			t.Errorf("expected %q to be valid", c)
		}
	}
}

// TestValidGTIDLiteral_RejectsInjection guards against any character that
// could break out of the embedded single-quoted SQL literal in
// SkipNextTransaction. The set here is the minimum that MUST be rejected.
func TestValidGTIDLiteral_RejectsInjection(t *testing.T) {
	cases := []string{
		"",                                 // empty
		"abc:1; DROP TABLE foo",            // statement injection
		"abc:1' OR '1'='1",                 // quote escape
		"abc:1\\",                          // backslash
		"abc:1 -- comment",                 // space
		"abc:1\nBEGIN;COMMIT",              // newline
		"abc:1/*comment*/",                 // block comment
		"3E11FA47-71CA-11E1-9E33-XYZAA9", // 'x','y','z' outside hex
	}
	for _, c := range cases {
		if validGTIDLiteral(c) {
			t.Errorf("expected %q to be REJECTED", c)
		}
	}
}
