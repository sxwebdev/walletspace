package main

import "testing"

func TestUIURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want string
	}{
		{name: "loopback", addr: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "hostname", addr: "localhost:9000", want: "http://localhost:9000"},
		// Wildcards are not routable, so the browser gets the loopback instead.
		{name: "ipv4 wildcard", addr: "0.0.0.0:8080", want: "http://127.0.0.1:8080"},
		{name: "empty host", addr: ":8080", want: "http://127.0.0.1:8080"},
		// Splitting on the first colon mangled this into "http://[:]:]:8080",
		// and the wildcard substitution never ran.
		{name: "ipv6 wildcard", addr: "[::]:8080", want: "http://127.0.0.1:8080"},
		{name: "ipv6 loopback", addr: "[::1]:8080", want: "http://[::1]:8080"},
		{name: "unparsable falls back verbatim", addr: "not-an-address", want: "http://not-an-address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := uiURL(tt.addr); got != tt.want {
				t.Errorf("uiURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}
