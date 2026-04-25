package pxc

import "testing"

func TestCountGTIDTransactions(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int64
	}{
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name:  "single transaction",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:5",
			want:  1,
		},
		{
			name:  "simple range",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100",
			want:  100,
		},
		{
			name:  "two disjoint ranges same UUID",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100:200-300",
			want:  201, // 100 + 101
		},
		{
			name: "two UUIDs",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-50," +
				"5C1A0CE7-1B6D-4C7A-9EBF-F88B7CFA0E11:1-25",
			want: 75,
		},
		{
			name: "MySQL newline in set",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-10,\n" +
				"5C1A0CE7-1B6D-4C7A-9EBF-F88B7CFA0E11:1-5",
			want: 15,
		},
		{
			name:  "single transaction at N=1",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1",
			want:  1,
		},
		{
			name:  "range where lo == hi",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:7-7",
			want:  1,
		},
		{
			name:  "fully caught up (empty subtract result)",
			input: "",
			want:  0,
		},
		{
			name: "mixed single and range",
			input: "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5:10:20-22",
			want:  9, // 5 + 1 + 3
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CountGTIDTransactions(tc.input)
			if got != tc.want {
				t.Errorf("CountGTIDTransactions(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}
