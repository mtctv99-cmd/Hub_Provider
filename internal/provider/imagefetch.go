package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ValidateImageURL checks that a remote image URL may be fetched. It rejects
// credentials in the URL, fragments, IP literals, hostnames that cannot be
// public, and ports other than the scheme default. allowHTTP widens the scheme
// check for providers whose upstream accepts plain http; the DNS/IP check in
// ImageTransport still applies.
func ValidateImageURL(rawURL string, allowHTTP bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Fragment != "" || u.Hostname() == "" || strings.HasSuffix(u.Hostname(), ".") {
		return fmt.Errorf("%w: malformed image URL", ErrInvalidImageURL)
	}
	switch u.Scheme {
	case "https":
		if u.Port() != "" && u.Port() != "443" {
			return fmt.Errorf("%w: nonstandard image port", ErrInvalidImageURL)
		}
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%w: image URL must use https", ErrInvalidImageURL)
		}
		if u.Port() != "" && u.Port() != "80" && u.Port() != "443" {
			return fmt.Errorf("%w: nonstandard image port", ErrInvalidImageURL)
		}
	default:
		return fmt.Errorf("%w: unsupported image URL scheme", ErrInvalidImageURL)
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return fmt.Errorf("%w: image IP literals are not allowed", ErrInvalidImageURL)
	}
	if !strings.Contains(u.Hostname(), ".") || strings.ContainsAny(u.Hostname(), "\\%") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".localhost") {
		return fmt.Errorf("%w: image host is invalid", ErrInvalidImageURL)
	}
	return nil
}

// PublicImageIP reports whether ip is a globally routable address, so DNS
// resolution cannot be pointed at loopback, private, link-local or CGNAT space.
func PublicImageIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is4In6() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "fc00::/7"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}

// ImageTransport dials only public IPs and ignores HTTP_PROXY so a proxy cannot
// be used to bypass the address check.
func ImageTransport() *http.Transport {
	return &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid image address")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("unable to resolve image host")
		}
		for _, ip := range ips {
			if !PublicImageIP(ip) {
				return nil, fmt.Errorf("%w: image host resolves to a nonpublic IP", ErrInvalidImageURL)
			}
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}}
}

// NewImageClient builds a guarded client for remote images: every request and
// redirect is re-validated, redirects are capped, and an https→http downgrade is
// refused even when allowHTTP is set. The returned cleanup closes idle
// connections; callers enforce their own body byte cap with io.LimitReader.
func NewImageClient(timeout time.Duration, allowHTTP bool) (*http.Client, func()) {
	transport := ImageTransport()
	client := &http.Client{Timeout: timeout, Transport: transport}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return fmt.Errorf("%w: too many image redirects", ErrInvalidImageURL)
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme == "http" {
			return fmt.Errorf("%w: image redirect downgrades to http", ErrInvalidImageURL)
		}
		return ValidateImageURL(req.URL.String(), allowHTTP)
	}
	return client, transport.CloseIdleConnections
}

// ImageMimeType keeps a declared image/* type (so avif/heic survive) and sniffs
// the bytes otherwise. Any non-image result is rejected.
func ImageMimeType(contentType string, data []byte) (string, error) {
	mimeType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if !strings.HasPrefix(mimeType, "image/") {
		mimeType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return "", fmt.Errorf("%w: remote content is not an image", ErrInvalidImageURL)
	}
	return mimeType, nil
}
