// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !release

// Package helpers contains small functions usable by any other
// package, both for testing or not.
package helpers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kylelemons/godebug/pretty"
	"github.com/mitchellh/mapstructure"
	"gopkg.in/yaml.v2"
)

var prettyC = pretty.Config{
	Diffable:          true,
	PrintStringers:    false,
	SkipZeroFields:    true,
	IncludeUnexported: false,
	Formatter: map[reflect.Type]interface{}{
		reflect.TypeOf(net.IP{}):            fmt.Sprint,
		reflect.TypeOf(netip.Addr{}):        fmt.Sprint,
		reflect.TypeOf(time.Time{}):         fmt.Sprint,
		reflect.TypeOf(SubnetMap[string]{}): fmt.Sprint,
	},
}

// DiffOption changes the behavior of the Diff function.
type DiffOption struct {
	kind int
	// When this is a formatter
	t  reflect.Type
	fn interface{}
}

// Diff return a diff of two objects. If no diff, an empty string is
// returned.
func Diff(a, b interface{}, options ...DiffOption) string {
	prettyC = prettyC
	for _, option := range options {
		switch option.kind {
		case DiffUnexported.kind:
			prettyC.IncludeUnexported = true
		case DiffZero.kind:
			prettyC.SkipZeroFields = false
		case DiffFormatter(nil, nil).kind:
			prettyC.Formatter[option.t] = option.fn
		}
	}
	return prettyC.Compare(a, b)
}

var (
	// DiffUnexported will display diff of unexported fields too.
	DiffUnexported = DiffOption{kind: 1}
	// DiffZero will include zero-field in diff
	DiffZero = DiffOption{kind: 2}
)

// DiffFormatter adds a new formatter
func DiffFormatter(t reflect.Type, fn interface{}) DiffOption {
	return DiffOption{kind: 3, t: t, fn: fn}
}

// HTTPEndpointCases describes case for TestHTTPEndpoints
type HTTPEndpointCases []struct {
	Description string
	Method      string
	URL         string
	Header      http.Header
	JSONInput   interface{}

	ContentType string
	StatusCode  int
	FirstLines  []string
	JSONOutput  interface{}
}

// TestHTTPEndpoints test a few HTTP endpoints
func TestHTTPEndpoints(t *testing.T, serverAddr net.Addr, cases HTTPEndpointCases) {
	t.Helper()
	for _, tc := range cases {
		desc := tc.Description
		if desc == "" {
			desc = tc.URL
		}
		t.Run(desc, func(t *testing.T) {
			t.Helper()
			if tc.FirstLines != nil && tc.JSONOutput != nil {
				t.Fatalf("Cannot have both FirstLines and JSONOutput")
			}
			var resp *http.Response
			var err error
			if tc.Method == "" {
				if tc.JSONInput == nil {
					tc.Method = "GET"
				} else {
					tc.Method = "POST"
				}
			}
			if tc.JSONInput == nil {
				req, _ := http.NewRequest(tc.Method,
					fmt.Sprintf("http://%s%s", serverAddr, tc.URL),
					nil)
				if tc.Header != nil {
					req.Header = tc.Header
				}
				resp, err = http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s %s:\n%+v", tc.Method, tc.URL, err)
				}
			} else {
				payload := new(bytes.Buffer)
				err = json.NewEncoder(payload).Encode(tc.JSONInput)
				if err != nil {
					t.Fatalf("Encode() error:\n%+v", err)
				}
				req, _ := http.NewRequest(tc.Method,
					fmt.Sprintf("http://%s%s", serverAddr, tc.URL),
					payload)
				if tc.Header != nil {
					req.Header = tc.Header
				}
				req.Header.Add("Content-Type", "application/json")
				resp, err = http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s %s:\n%+v", tc.Method, tc.URL, err)
				}
			}

			defer resp.Body.Close()
			if tc.StatusCode == 0 {
				tc.StatusCode = 200
			}
			if resp.StatusCode != tc.StatusCode {
				t.Errorf("%s %s: got status code %d, not %d", tc.URL,
					tc.Method, resp.StatusCode, tc.StatusCode)
			}
			if tc.JSONOutput != nil {
				tc.ContentType = "application/json; charset=utf-8"
			}
			gotContentType := resp.Header.Get("Content-Type")
			if gotContentType != tc.ContentType {
				t.Errorf("%s %s Content-Type (-got, +want):\n-%s\n+%s",
					tc.Method, tc.URL, gotContentType, tc.ContentType)
			}
			if tc.JSONOutput == nil {
				reader := bufio.NewScanner(resp.Body)
				got := []string{}
				for reader.Scan() && len(got) < len(tc.FirstLines) {
					got = append(got, reader.Text())
				}
				if diff := Diff(got, tc.FirstLines); diff != "" {
					t.Errorf("%s %s (-got, +want):\n%s", tc.Method, tc.URL, diff)
				}
			} else {
				decoder := json.NewDecoder(resp.Body)
				var got gin.H
				if err := decoder.Decode(&got); err != nil {
					t.Fatalf("%s %s:\n%+v", tc.Method, tc.URL, err)
				}
				if diff := Diff(got, tc.JSONOutput); diff != "" {
					t.Fatalf("%s %s (-got, +want):\n%s", tc.Method, tc.URL, diff)
				}
			}
		})
	}
}

// ConfigurationDecodeCases describes a test case for configuration
// decode. We use functions to return value as the decoding process
// may mutate the configuration.
type ConfigurationDecodeCases []struct {
	Description   string
	Initial       func() interface{} // initial value for configuration
	Configuration func() interface{} // configuration to decode
	Expected      interface{}
	Error         bool
}

// TestConfigurationDecode helps decoding configuration. It also test decoding from YAML.
func TestConfigurationDecode(t *testing.T, cases ConfigurationDecodeCases, options ...DiffOption) {
	t.Helper()
	for _, tc := range cases {
		for _, fromYAML := range []bool{false, true} {
			title := tc.Description
			if fromYAML {
				title = fmt.Sprintf("%s (from YAML)", title)
				if tc.Configuration == nil {
					continue
				}
			}
			t.Run(title, func(t *testing.T) {
				var configuration interface{}
				if fromYAML {
					// Encode and decode with YAML
					out, err := yaml.Marshal(tc.Configuration())
					if err != nil {
						t.Fatalf("yaml.Marshal() error:\n%+v", err)
					}
					if err := yaml.Unmarshal(out, &configuration); err != nil {
						t.Fatalf("yaml.Unmarshal() error:\n%+v", err)
					}
				} else {
					// Just use as is
					configuration = tc.Configuration()
				}
				got := tc.Initial()

				decoder, err := mapstructure.NewDecoder(GetMapStructureDecoderConfig(&got))
				if err != nil {
					t.Fatalf("NewDecoder() error:\n%+v", err)
				}
				err = decoder.Decode(configuration)
				if err != nil && !tc.Error {
					t.Fatalf("Decode() error:\n%+v", err)
				} else if err == nil && tc.Error {
					t.Errorf("Decode() did not error")
				}

				if diff := Diff(got, tc.Expected, options...); diff != "" && err == nil {
					t.Fatalf("Decode() (-got, +want):\n%s", diff)
				}
			})
		}
	}
}

// CheckExternalService checks an external service, available either
// as a named service or on a specific port on localhost. This applies
// for example for Kafka and ClickHouse. The timeouts are quite short,
// but we suppose that either the services are run through
// docker-compose manually and ready, either through CI and they are
// checked for readiness.
func CheckExternalService(t *testing.T, name string, dnsCandidates []string, port string) string {
	t.Helper()
	if testing.Short() {
		t.Skipf("Skip test with real %s in short mode", name)
	}
	mandatory := os.Getenv("CI_AKVORADO_FUNCTIONAL_TESTS") != ""
	var err error

	found := ""
	for _, dnsCandidate := range dnsCandidates {
		resolv := net.Resolver{PreferGo: true}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err = resolv.LookupHost(ctx, dnsCandidate)
		cancel()
		if err == nil {
			found = dnsCandidate
			break
		}
	}
	if found == "" {
		if mandatory {
			t.Fatalf("%s cannot be resolved (CI_AKVORADO_FUNCTIONAL_TESTS is set)", name)
		}
		t.Skipf("%s cannot be resolved (CI_AKVORADO_FUNCTIONAL_TESTS is not set)", name)
	}

	var d net.Dialer
	server := net.JoinHostPort(found, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	for {
		_, err := d.DialContext(ctx, "tcp", server)
		if err == nil {
			break
		}
		if mandatory {
			t.Logf("DialContext() error:\n%+v", err)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if mandatory {
				t.Fatalf("%s is not running (CI_AKVORADO_FUNCTIONAL_TESTS is set)", name)
			} else {
				t.Skipf("%s is not running (CI_AKVORADO_FUNCTIONAL_TESTS is not set)", name)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()

	return server
}

// StartStop starts a component and stops it on cleanup.
func StartStop(t *testing.T, component interface{}) {
	t.Helper()
	if starterC, ok := component.(starter); ok {
		if err := starterC.Start(); err != nil {
			t.Fatalf("Start() error:\n%+v", err)
		}
	}
	t.Cleanup(func() {
		if stopperC, ok := component.(stopper); ok {
			if err := stopperC.Stop(); err != nil {
				t.Errorf("Stop() error:\n%+v", err)
			}
		}
	})
}

type starter interface {
	Start() error
}
type stopper interface {
	Stop() error
}
