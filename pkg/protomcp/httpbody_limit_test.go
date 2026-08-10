package protomcp

import "testing"

func TestHTTPBodyDocumentLimit(t *testing.T) {
	cases := []struct {
		name string
		opts []ServerOption
		want int
	}{
		{"unset uses the default", nil, DefaultHTTPBodyDocumentLimit},
		{"positive override is returned verbatim", []ServerOption{WithHTTPBodyDocumentLimit(1234)}, 1234},
		{"negative disables the cap", []ServerOption{WithHTTPBodyDocumentLimit(-1)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New("test", "0.0.1", tc.opts...)
			if got := s.HTTPBodyDocumentLimit(); got != tc.want {
				t.Errorf("HTTPBodyDocumentLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}
