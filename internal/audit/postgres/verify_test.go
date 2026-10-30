package postgres

import "testing"

func TestBytesEqualNilAware(t *testing.T) {
	cases := []struct {
		name string
		a, b []byte
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs empty", nil, []byte{}, true},
		{"equal", []byte{1, 2, 3}, []byte{1, 2, 3}, true},
		{"different length", []byte{1, 2}, []byte{1, 2, 3}, false},
		{"same length different content", []byte{1, 2, 3}, []byte{1, 2, 4}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bytesEqualNilAware(c.a, c.b); got != c.want {
				t.Errorf("bytesEqualNilAware(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
