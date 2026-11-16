// Copyright (C) 2019-2026 Chrystian Huot <chrystian.huot@saubeo.solutions>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>

package cloudstore

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestRegionOfReadsTheRegionTheEndpointNames(t *testing.T) {
	cases := map[string]string{
		"s3.us-east-005.backblazeb2.com":        "us-east-005",
		"s3.eu-central-003.backblazeb2.com:443": "eu-central-003",
		"abc123.r2.cloudflarestorage.com":       "auto",
		"s3.eu-west-1.amazonaws.com":            "eu-west-1",
		"s3.amazonaws.com":                      "us-east-1",
		"127.0.0.1:9000":                        "us-east-1",
	}

	for host, want := range cases {
		if got := regionOf(host); got != want {
			t.Errorf("regionOf(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestNewRefusesAConfigurationItCannotUse(t *testing.T) {
	good := Config{Endpoint: "https://s3.us-east-005.backblazeb2.com/", Bucket: "b", KeyID: "k", AppKey: "s"}

	client, err := New(good)
	if err != nil {
		t.Fatalf("a good configuration: %v", err)
	}

	if client.host != "s3.us-east-005.backblazeb2.com" || client.scheme != "https" {
		t.Errorf("the endpoint read as %s://%s", client.scheme, client.host)
	}

	for name, bad := range map[string]Config{
		"no endpoint":      {Bucket: "b", KeyID: "k", AppKey: "s"},
		"endpoint path":    {Endpoint: "s3.example.com/b", Bucket: "b", KeyID: "k", AppKey: "s"},
		"no bucket":        {Endpoint: "s3.example.com", KeyID: "k", AppKey: "s"},
		"bucket with path": {Endpoint: "s3.example.com", Bucket: "a/b", KeyID: "k", AppKey: "s"},
		"no key id":        {Endpoint: "s3.example.com", Bucket: "b", AppKey: "s"},
		"no app key":       {Endpoint: "s3.example.com", Bucket: "b", KeyID: "k"},
	} {
		if _, err := New(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReasonSaysWhyInTheOperatorsWords(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&unreachable{errors.New("dial tcp: refused")}, "the endpoint couldn't be reached"},
		{&Error{Status: http.StatusNotFound, Code: "NoSuchBucket"}, "the bucket wasn't found"},
		{&Error{Status: http.StatusForbidden, Code: "InvalidAccessKeyId"}, "the key was refused"},
		{&Error{Status: http.StatusForbidden, Code: "SignatureDoesNotMatch"}, "the key was refused"},
		{&Error{Status: http.StatusUnauthorized, Code: "Unauthorized"}, "the key was refused"},
		{&Error{Status: http.StatusForbidden, Code: "AccessDenied"}, "the key can't read, write, or delete in this bucket"},
		{&Error{Status: http.StatusServiceUnavailable}, "the endpoint couldn't be reached"},
		{fmt.Errorf("putting: %w", &Error{Status: http.StatusForbidden, Code: "AccessDenied"}), "the key can't read, write, or delete in this bucket"},
	}

	for _, c := range cases {
		if got := Reason(c.err); got != c.want {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestURIEncodeIsRFC3986(t *testing.T) {
	if got := uriEncode("a b/c+d=e~f_g.h-i"); got != "a%20b%2Fc%2Bd%3De~f_g.h-i" {
		t.Errorf("uriEncode = %q", got)
	}
}
