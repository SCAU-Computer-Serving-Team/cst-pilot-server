package main

import "testing"

func TestProductionAuthConfiguration(t *testing.T) {
	cases := []struct {
		name  string
		opts  authOptions
		valid bool
	}{
		{"missing", authOptions{}, false},
		{"explicit-test", authOptions{AllowStub: true}, true},
		{"service-key-required", authOptions{URL: "https://www.cstoa.top/api/oauth/introspect"}, false},
		{"https", authOptions{URL: "https://www.cstoa.top/api/oauth/introspect", ServiceToken: "test"}, true},
		{"loopback", authOptions{URL: "http://127.0.0.1:8080/api/oauth/introspect", ServiceToken: "test"}, true},
		{"remote-plaintext", authOptions{URL: "http://public.example/introspect", ServiceToken: "test"}, false},
		{"embedded-password", authOptions{URL: "https://user:password@example.test/introspect", ServiceToken: "test"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if valid := validateAuthOptions(tc.opts) == nil; valid != tc.valid {
				t.Fatalf("configuration valid=%v, expected %v", valid, tc.valid)
			}
		})
	}
}
