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

// Package cloudstore talks to an S3-compatible bucket - Backblaze B2, Cloudflare R2, Amazon S3 -
// with the few requests the audio storage needs: put, get, head, list and delete. It signs with
// AWS Signature Version 4 and addresses the bucket path-style, which every one of them accepts.
package cloudstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config names a bucket and the key that opens it.
type Config struct {
	Endpoint string // host, with or without https://, e.g. s3.us-east-005.backblazeb2.com
	Bucket   string
	KeyID    string
	AppKey   string
}

// Client is one bucket, ready for requests.
type Client struct {
	scheme, host, region, bucket string
	keyID, appKey                string

	http *http.Client
	now  func() time.Time
}

// ErrNotFound is a key the bucket does not hold.
var ErrNotFound = errors.New("cloudstore: not found")

// Error is a request the bucket answered with an error.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("cloudstore: %d %s: %s", e.Status, e.Code, e.Message)
}

// New checks a configuration and builds the client for it.
func New(config Config) (*Client, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	scheme := "https"

	if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint = rest
	} else if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint, scheme = rest, "http"
	}

	endpoint = strings.TrimRight(endpoint, "/")

	switch {
	case endpoint == "", strings.ContainsAny(endpoint, "/?# "):
		return nil, fmt.Errorf("cloudstore: endpoint %q is not a host", config.Endpoint)
	case strings.TrimSpace(config.Bucket) == "", strings.ContainsAny(config.Bucket, "/?# "):
		return nil, fmt.Errorf("cloudstore: bucket %q is not a bucket name", config.Bucket)
	case strings.TrimSpace(config.KeyID) == "" || config.AppKey == "":
		return nil, errors.New("cloudstore: a key ID and an application key are both needed")
	}

	return &Client{
		scheme: scheme,
		host:   endpoint,
		region: regionOf(endpoint),
		bucket: strings.TrimSpace(config.Bucket),
		keyID:  strings.TrimSpace(config.KeyID),
		appKey: config.AppKey,
		http:   &http.Client{Timeout: 2 * time.Minute},
		now:    time.Now,
	}, nil
}

// regionOf is the signing region an endpoint answers for: the one its name carries
// (s3.us-east-005.backblazeb2.com, s3.eu-west-1.amazonaws.com), "auto" for Cloudflare R2,
// and us-east-1, the S3 default, for anything else.
func regionOf(host string) string {
	name := strings.ToLower(host)

	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}

	if strings.HasSuffix(name, ".r2.cloudflarestorage.com") {
		return "auto"
	}

	if rest, ok := strings.CutPrefix(name, "s3."); ok {
		if region, _, found := strings.Cut(rest, "."); found && region != "amazonaws" {
			return region
		}
	}

	return "us-east-1"
}

// Put stores one object.
func (client *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	header := http.Header{}

	if contentType != "" {
		header.Set("Content-Type", contentType)
	}

	res, err := client.do(ctx, http.MethodPut, key, nil, header, body)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	return answerOf(res)
}

// Get reads one object, or answers ErrNotFound.
func (client *Client) Get(ctx context.Context, key string) ([]byte, error) {
	res, err := client.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()

	if err := answerOf(res); err != nil {
		return nil, err
	}

	return io.ReadAll(res.Body)
}

// Head answers the size of one object, or ErrNotFound.
func (client *Client) Head(ctx context.Context, key string) (int64, error) {
	res, err := client.do(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return 0, err
	}

	defer res.Body.Close()

	if err := answerOf(res); err != nil {
		return 0, err
	}

	return res.ContentLength, nil
}

// Object is one listed object.
type Object struct {
	Key  string
	Size int64
}

// Page is one page of a listing: the objects, the common prefixes a delimiter folded,
// and the token that reads the next page, empty on the last one.
type Page struct {
	Objects  []Object
	Prefixes []string
	Next     string
}

// List reads one page of the objects under a prefix.
func (client *Client) List(ctx context.Context, prefix, delimiter, token string) (Page, error) {
	query := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}

	if prefix != "" {
		query.Set("prefix", prefix)
	}

	if delimiter != "" {
		query.Set("delimiter", delimiter)
	}

	if token != "" {
		query.Set("continuation-token", token)
	}

	res, err := client.do(ctx, http.MethodGet, "", query, nil, nil)
	if err != nil {
		return Page{}, err
	}

	defer res.Body.Close()

	if err := answerOf(res); err != nil {
		return Page{}, err
	}

	var answer struct {
		Contents []struct {
			Key  string
			Size int64
		}
		CommonPrefixes []struct {
			Prefix string
		}
		IsTruncated           bool
		NextContinuationToken string
	}

	if err := xml.NewDecoder(res.Body).Decode(&answer); err != nil {
		return Page{}, fmt.Errorf("cloudstore: reading a listing: %w", err)
	}

	page := Page{}

	for _, object := range answer.Contents {
		page.Objects = append(page.Objects, Object{Key: object.Key, Size: object.Size})
	}

	for _, prefix := range answer.CommonPrefixes {
		page.Prefixes = append(page.Prefixes, prefix.Prefix)
	}

	if answer.IsTruncated {
		page.Next = answer.NextContinuationToken
	}

	return page, nil
}

// Delete removes up to 1000 objects in one request. A key the bucket does not hold is no error.
func (client *Client) Delete(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	if len(keys) > 1000 {
		return errors.New("cloudstore: at most 1000 keys go in one delete")
	}

	type object struct {
		Key string `xml:"Key"`
	}

	body := struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []object `xml:"Object"`
	}{Quiet: true}

	for _, key := range keys {
		body.Objects = append(body.Objects, object{Key: key})
	}

	payload, err := xml.Marshal(body)
	if err != nil {
		return err
	}

	sum := md5.Sum(payload)

	header := http.Header{}
	header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	header.Set("Content-Type", "application/xml")

	res, err := client.do(ctx, http.MethodPost, "", url.Values{"delete": {""}}, header, payload)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	if err := answerOf(res); err != nil {
		return err
	}

	// A quiet delete answers only the keys it could not remove.
	var answer struct {
		Errors []struct {
			Key     string
			Code    string
			Message string
		} `xml:"Error"`
	}

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if len(bytes.TrimSpace(raw)) > 0 {
		if err := xml.Unmarshal(raw, &answer); err != nil {
			return fmt.Errorf("cloudstore: reading a delete answer: %w", err)
		}
	}

	for _, failed := range answer.Errors {
		if failed.Code == "NoSuchKey" {
			continue
		}

		return &Error{Status: http.StatusOK, Code: failed.Code, Message: failed.Key + ": " + failed.Message}
	}

	return nil
}

// do signs and sends one request. An empty key addresses the bucket itself.
func (client *Client) do(ctx context.Context, method, key string, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	path := "/" + client.bucket

	if key != "" {
		path += "/" + key
	}

	target := url.URL{Scheme: client.scheme, Host: client.host, Path: path, RawPath: escapePath(path), RawQuery: canonicalQuery(query)}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for name, values := range header {
		req.Header[name] = values
	}

	req.ContentLength = int64(len(body))

	client.sign(req, path, query, body)

	res, err := client.http.Do(req)
	if err != nil {
		return nil, &unreachable{err}
	}

	return res, nil
}

// unreachable is a request that never got an answer.
type unreachable struct{ err error }

func (u *unreachable) Error() string { return "cloudstore: unreachable: " + u.err.Error() }
func (u *unreachable) Unwrap() error { return u.err }

// answerOf turns a response that is not a success into the error it carries.
func answerOf(res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))

	var answer struct {
		Code    string
		Message string
	}

	_ = xml.Unmarshal(raw, &answer)

	if res.StatusCode == http.StatusNotFound && (answer.Code == "" || answer.Code == "NoSuchKey") {
		return ErrNotFound
	}

	return &Error{Status: res.StatusCode, Code: answer.Code, Message: answer.Message}
}

// Reason says in the operator's words why a request failed: the four reasons the
// Test connection button and the log name.
func Reason(err error) string {
	var unreached *unreachable
	var answered *Error

	switch {
	case err == nil:
		return ""
	case errors.As(err, &unreached):
		return "the endpoint couldn't be reached"
	case errors.As(err, &answered):
		switch {
		case answered.Code == "NoSuchBucket":
			return "the bucket wasn't found"
		case answered.Code == "InvalidAccessKeyId", answered.Code == "SignatureDoesNotMatch",
			answered.Code == "InvalidToken", answered.Code == "ExpiredToken", answered.Status == http.StatusUnauthorized:
			return "the key was refused"
		case answered.Code == "AccessDenied", answered.Status == http.StatusForbidden:
			return "the key can't read, write, or delete in this bucket"
		case answered.Status >= 500:
			// The bucket's service is down or overloaded: from here it is out of reach.
			return "the endpoint couldn't be reached"
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return "the endpoint couldn't be reached"
	}

	return err.Error()
}

const (
	signAlgorithm = "AWS4-HMAC-SHA256"
	signService   = "s3"
)

// sign adds the Signature Version 4 headers to a request.
func (client *Client) sign(req *http.Request, path string, query url.Values, body []byte) {
	now := client.now().UTC()
	stamp := now.Format("20060102T150405Z")
	day := now.Format("20060102")

	payloadHash := sha256Hex(body)

	req.Header.Set("Host", client.host)
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	names := make([]string, 0, len(req.Header))
	values := map[string]string{}

	for name, vals := range req.Header {
		lower := strings.ToLower(name)

		if lower != "host" && lower != "content-type" && lower != "content-md5" && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}

		names = append(names, lower)
		values[lower] = strings.Join(trimAll(vals), ",")
	}

	sort.Strings(names)

	var canonicalHeaders strings.Builder

	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + values[name] + "\n")
	}

	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		escapePath(path),
		canonicalQuery(query),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := day + "/" + client.region + "/" + signService + "/aws4_request"

	toSign := strings.Join([]string{signAlgorithm, stamp, scope, sha256Hex([]byte(canonical))}, "\n")

	key := hmacSHA256([]byte("AWS4"+client.appKey), day)
	key = hmacSHA256(key, client.region)
	key = hmacSHA256(key, signService)
	key = hmacSHA256(key, "aws4_request")

	signature := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", signAlgorithm+" Credential="+client.keyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)

	// Go sends the host from the URL, never from the header map.
	req.Host = client.host
}

func trimAll(values []string) []string {
	out := make([]string, len(values))

	for i, value := range values {
		out[i] = strings.Join(strings.Fields(value), " ")
	}

	return out
}

// escapePath encodes each segment of a path the way Signature Version 4 reads it.
func escapePath(path string) string {
	segments := strings.Split(path, "/")

	for i, segment := range segments {
		segments[i] = uriEncode(segment)
	}

	return strings.Join(segments, "/")
}

// canonicalQuery is a query sorted and encoded the way Signature Version 4 reads it.
func canonicalQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}

	keys := make([]string, 0, len(query))

	for key := range query {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	parts := []string{}

	for _, key := range keys {
		values := append([]string(nil), query[key]...)

		sort.Strings(values)

		for _, value := range values {
			parts = append(parts, uriEncode(key)+"="+uriEncode(value))
		}
	}

	return strings.Join(parts, "&")
}

// uriEncode is RFC 3986 encoding: every byte but the unreserved set, upper-case hex.
func uriEncode(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]

		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)

			continue
		}

		b.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(c)>>4, 16)) + strings.ToUpper(strconv.FormatUint(uint64(c)&15, 16)))
	}

	return b.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))

	return mac.Sum(nil)
}

// Probe checks that the bucket can be written to, read from and deleted from, with a
// small object it removes again.
func (client *Client) Probe(ctx context.Context) error {
	key := fmt.Sprintf("rdio-scanner-connection-test/%d.txt", client.now().UnixNano())
	body := []byte("rdio-scanner connection test")

	if err := client.Put(ctx, key, body, "text/plain"); err != nil {
		return err
	}

	got, err := client.Get(ctx, key)
	if err != nil {
		return err
	}

	if !bytes.Equal(got, body) {
		return errors.New("cloudstore: the test object came back different")
	}

	return client.Delete(ctx, []string{key})
}
