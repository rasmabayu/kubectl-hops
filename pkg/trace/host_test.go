package trace

import "testing"

func TestParseHost(t *testing.T) {
	tests := []struct {
		in, host, path string
		wantErr        bool
	}{
		{in: "", host: "", path: ""},
		{in: "shop.example.com", host: "shop.example.com"},
		{in: "  Shop.Example.COM. ", host: "shop.example.com"},
		{in: "shop.example.com:8443", host: "shop.example.com"},
		{in: "https://docs.example.com", host: "docs.example.com"},
		{in: "https://docs.example.com/", host: "docs.example.com", path: "/"},
		{in: "http://shop.example.com:8080/api/v1?x=1#top", host: "shop.example.com", path: "/api/v1"},
		{in: "shop.example.com/cart", host: "shop.example.com", path: "/cart"},
		{in: "https://", wantErr: true},
		{in: "10.0.0.1", wantErr: true},
		{in: "http://[::1]:80/", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			host, path, err := ParseHost(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && (host != tc.host || path != tc.path) {
				t.Errorf("got (%q, %q), want (%q, %q)", host, path, tc.host, tc.path)
			}
		})
	}
}
