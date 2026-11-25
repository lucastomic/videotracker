package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // logo decoders
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
	"golang.org/x/net/html"
)

const (
	fetchTimeout      = 8 * time.Second
	fetchMaxBytes     = 3 << 20
	fetchMaxRedirects = 5
	browserUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defaultBrandColor = "#0B2545"
)

var errBlockedAddress = errors.New("dirección no permitida (privada, loopback, link-local o metadata)")

// Extra ranges not covered by netip.Addr helpers.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64
	netip.MustParsePrefix("2001:db8::/32"),
}

// isBlockedIP reports whether the server must never connect to ip.
func isBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// safeDialContext resolves the host itself and only connects to public IPs,
// dialing the checked IP directly (no second DNS lookup => no rebinding).
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: fetchTimeout}
	var lastErr error = errBlockedAddress
	for _, ip := range ips {
		if isBlockedIP(ip) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if errors.Is(lastErr, errBlockedAddress) {
		return nil, fmt.Errorf("%s: %w", host, errBlockedAddress)
	}
	return nil, lastErr
}

var safeHTTPClient = &http.Client{
	Timeout: fetchTimeout,
	Transport: &http.Transport{
		Proxy:                 nil, // never go through an env proxy: it would bypass the IP checks
		DialContext:           safeDialContext,
		TLSHandshakeTimeout:   fetchTimeout,
		ResponseHeaderTimeout: fetchTimeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= fetchMaxRedirects {
			return fmt.Errorf("demasiadas redirecciones (máx %d)", fetchMaxRedirects)
		}
		return checkFetchURL(req.URL)
	},
}

func checkFetchURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("esquema no permitido: %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("URL sin host")
	}
	if u.User != nil {
		return errors.New("URL con credenciales no permitida")
	}
	// Literal IPs are checked here too (the dialer checks resolved names).
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && isBlockedIP(ip) {
		return fmt.Errorf("%s: %w", u.Hostname(), errBlockedAddress)
	}
	return nil
}

// safeFetch GETs a public http(s) URL with SSRF protections, an 8s timeout
// and a 3 MB body limit. It returns the body and the final URL.
func safeFetch(ctx context.Context, rawURL string) ([]byte, *url.URL, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, nil, "", fmt.Errorf("URL no válida: %w", err)
	}
	if err := checkFetchURL(u); err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, "", err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,image/avif,image/webp,image/png,image/*;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "es-ES,es;q=0.9,en;q=0.6")
	resp, err := safeHTTPClient.Do(req)
	if err != nil {
		return nil, nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, "", fmt.Errorf("HTTP %d en %s", resp.StatusCode, resp.Request.URL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
	if err != nil {
		return nil, nil, "", err
	}
	if len(body) > fetchMaxBytes {
		return nil, nil, "", fmt.Errorf("respuesta demasiado grande (> %d bytes)", fetchMaxBytes)
	}
	return body, resp.Request.URL, resp.Header.Get("Content-Type"), nil
}

// ===== Metadata extraction =====

// SiteMeta is what we learn about a company's website.
type SiteMeta struct {
	Name           string   `json:"company_name"`
	LogoURL        string   `json:"company_logo_url"`
	BrandColor     string   `json:"brand_color"`
	ThemeColor     string   `json:"theme_color,omitempty"`
	LogoCandidates []string `json:"logo_candidates,omitempty"`
}

type iconCandidate struct {
	url   string
	score int
}

// extractMetadata parses a page and returns name, theme color and an
// ordered list of logo candidates (best first). Pure function: no network.
func extractMetadata(body []byte, pageURL *url.URL) SiteMeta {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return SiteMeta{}
	}
	base := pageURL
	var siteName, ogTitle, title, themeColor, ogImage string
	var icons []iconCandidate
	var headerLogos []string

	abs := func(ref string) string {
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.HasPrefix(ref, "data:") {
			return ""
		}
		u, err := base.Parse(ref)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		return u.String()
	}

	var walk func(n *html.Node, inHeader bool)
	walk = func(n *html.Node, inHeader bool) {
		if n.Type == html.ElementNode {
			a := attrMap(n)
			switch n.Data {
			case "base":
				if h := a["href"]; h != "" {
					if u, err := pageURL.Parse(h); err == nil {
						base = u
					}
				}
			case "title":
				if title == "" && n.FirstChild != nil {
					title = n.FirstChild.Data
				}
			case "meta":
				key := strings.ToLower(a["property"])
				if key == "" {
					key = strings.ToLower(a["name"])
				}
				switch key {
				case "og:site_name":
					siteName = a["content"]
				case "og:title":
					ogTitle = a["content"]
				case "og:image", "og:image:url", "og:image:secure_url":
					if ogImage == "" {
						ogImage = abs(a["content"])
					}
				case "theme-color":
					if themeColor == "" {
						themeColor = a["content"]
					}
				}
			case "link":
				rel := " " + strings.ToLower(a["rel"]) + " "
				href := abs(a["href"])
				if href == "" || isSVG(href, a["type"]) {
					break
				}
				size := parseIconSize(a["sizes"])
				switch {
				case strings.Contains(rel, "apple-touch-icon"):
					if size == 0 {
						size = 180
					}
					icons = append(icons, iconCandidate{href, 1000 + size})
				case strings.Contains(rel, " icon ") || strings.Contains(rel, "shortcut"):
					icons = append(icons, iconCandidate{href, size})
				}
			case "header":
				inHeader = true
			case "img":
				src := a["src"]
				if src == "" {
					src = firstSrcsetURL(a["srcset"])
				}
				if s := a["data-src"]; s != "" && (src == "" || strings.HasPrefix(src, "data:")) {
					src = s
				}
				hint := strings.ToLower(a["class"] + " " + a["id"] + " " + a["alt"] + " " + src)
				if u := abs(src); u != "" && !isSVG(u, "") && (strings.Contains(hint, "logo") || inHeader && len(headerLogos) == 0) {
					if strings.Contains(hint, "logo") {
						headerLogos = append([]string{u}, headerLogos...)
					} else {
						headerLogos = append(headerLogos, u)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inHeader)
		}
	}
	walk(doc, false)

	meta := SiteMeta{ThemeColor: normalizeHexColor(themeColor)}
	meta.Name = cleanSiteName(siteName, ogTitle, title, pageURL.Hostname())

	// Logo preference: apple-touch-icon > big icon (>=96px) > logo <img> in
	// the page/header > og:image > small favicons > /apple-touch-icon.png.
	sort.SliceStable(icons, func(i, j int) bool { return icons[i].score > icons[j].score })
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			meta.LogoCandidates = append(meta.LogoCandidates, u)
		}
	}
	for _, ic := range icons {
		if ic.score >= 96 {
			add(ic.url)
		}
	}
	for _, l := range headerLogos {
		add(l)
	}
	add(ogImage)
	for _, ic := range icons {
		add(ic.url)
	}
	if len(meta.LogoCandidates) > 9 {
		meta.LogoCandidates = meta.LogoCandidates[:9]
	}
	add(abs("/apple-touch-icon.png"))
	if len(meta.LogoCandidates) > 0 {
		meta.LogoURL = meta.LogoCandidates[0]
	}
	return meta
}

func attrMap(n *html.Node) map[string]string {
	m := make(map[string]string, len(n.Attr))
	for _, a := range n.Attr {
		m[strings.ToLower(a.Key)] = a.Val
	}
	return m
}

func isSVG(u, typ string) bool {
	l := strings.ToLower(u)
	if i := strings.IndexAny(l, "?#"); i >= 0 {
		l = l[:i]
	}
	return strings.HasSuffix(l, ".svg") || strings.Contains(strings.ToLower(typ), "svg")
}

func firstSrcsetURL(srcset string) string {
	first, _, _ := strings.Cut(srcset, ",")
	f := strings.Fields(first)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// parseIconSize returns the largest dimension in a sizes="32x32 64x64" attr.
func parseIconSize(s string) int {
	best := 0
	for _, f := range strings.Fields(strings.ToLower(s)) {
		if f == "any" {
			return 512
		}
		w, _, _ := strings.Cut(f, "x")
		if n, err := strconv.Atoi(w); err == nil && n > best {
			best = n
		}
	}
	return best
}

var titleSeparators = []string{" | ", " - ", " – ", " — ", " · ", " :: ", " » ", ": "}

// cleanSiteName picks og:site_name > og:title > <title> and strips taglines
// ("Marina Ibiza | Puerto deportivo" -> "Marina Ibiza"), preferring the part
// that looks like the domain.
func cleanSiteName(siteName, ogTitle, title, host string) string {
	clean := func(s string) string {
		s = html.UnescapeString(s)
		return strings.Join(strings.Fields(s), " ")
	}
	if s := clean(siteName); s != "" {
		return truncateRunes(s, 60)
	}
	raw := clean(ogTitle)
	if raw == "" {
		raw = clean(title)
	}
	if raw == "" {
		return hostLabel(host)
	}
	parts := []string{raw}
	for _, sep := range titleSeparators {
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, sep)...)
		}
		parts = next
	}
	domain := lettersOnly(hostLabel(host))
	best, bestScore := strings.TrimSpace(parts[0]), -1
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		score := commonLetters(lettersOnly(p), domain)
		if score > bestScore {
			best, bestScore = p, score
		}
	}
	return truncateRunes(best, 60)
}

// hostLabel turns "www.marinaibiza.com" into "marinaibiza".
func hostLabel(host string) string {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	label, _, _ := strings.Cut(host, ".")
	return label
}

func lettersOnly(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case '&':
			b.WriteString("and")
		case 'á', 'à':
			b.WriteRune('a')
		case 'é', 'è':
			b.WriteRune('e')
		case 'í':
			b.WriteRune('i')
		case 'ó', 'ò':
			b.WriteRune('o')
		case 'ú', 'ü':
			b.WriteRune('u')
		default:
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// commonLetters is the length of the longest common substring.
func commonLetters(a, b string) int {
	best := 0
	for i := 0; i < len(a); i++ {
		for j := 0; j < len(b); j++ {
			k := 0
			for i+k < len(a) && j+k < len(b) && a[i+k] == b[j+k] {
				k++
			}
			if k > best {
				best = k
			}
		}
	}
	return best
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// ===== Colors =====

// normalizeHexColor accepts #rgb, #rrggbb, rgb(r,g,b) and returns #RRGGBB or "".
func normalizeHexColor(s string) string {
	c, ok := parseColor(s)
	if !ok {
		return ""
	}
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}

func parseColor(s string) (color.RGBA, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "rgb") {
		open, close := strings.Index(s, "("), strings.Index(s, ")")
		if open < 0 || close < open {
			return color.RGBA{}, false
		}
		f := strings.FieldsFunc(s[open+1:close], func(r rune) bool { return r == ',' || r == ' ' || r == '/' })
		if len(f) < 3 {
			return color.RGBA{}, false
		}
		var v [3]uint8
		for i := 0; i < 3; i++ {
			n, err := strconv.Atoi(f[i])
			if err != nil || n < 0 || n > 255 {
				return color.RGBA{}, false
			}
			v[i] = uint8(n)
		}
		return color.RGBA{v[0], v[1], v[2], 255}, true
	}
	s = strings.TrimPrefix(s, "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return color.RGBA{}, false
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{}, false
	}
	return color.RGBA{uint8(n >> 16), uint8(n >> 8), uint8(n), 255}, true
}

func luminance(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// usableBrandColor rejects near-white / near-black / grey colors, which
// make for a dull thumbnail (many sites use theme-color #ffffff).
func usableBrandColor(c color.RGBA) bool {
	maxC := max(c.R, c.G, c.B)
	minC := min(c.R, c.G, c.B)
	l := luminance(c)
	return l < 0.85 && l > 0.004 && int(maxC)-int(minC) > 30
}

// dominantColor returns the most common saturated color of a logo.
func dominantColor(img image.Image) (color.RGBA, bool) {
	b := img.Bounds()
	step := max(1, max(b.Dx(), b.Dy())/128)
	type acc struct{ r, g, b, n int }
	buckets := map[uint16]*acc{}
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, a := img.At(x, y).RGBA()
			if a < 0xC000 {
				continue
			}
			c := color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255}
			if !usableBrandColor(c) {
				continue
			}
			k := uint16(c.R>>4)<<8 | uint16(c.G>>4)<<4 | uint16(c.B>>4)
			if buckets[k] == nil {
				buckets[k] = &acc{}
			}
			ac := buckets[k]
			ac.r += int(c.R)
			ac.g += int(c.G)
			ac.b += int(c.B)
			ac.n++
		}
	}
	var best *acc
	for _, ac := range buckets {
		if best == nil || ac.n > best.n {
			best = ac
		}
	}
	if best == nil || best.n < 4 {
		return color.RGBA{}, false
	}
	return color.RGBA{uint8(best.r / best.n), uint8(best.g / best.n), uint8(best.b / best.n), 255}, true
}

// fetchImage downloads and decodes an image through the safe client.
func fetchImage(ctx context.Context, u string) (image.Image, []byte, error) {
	body, _, _, err := safeFetch(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("imagen no decodificable: %w", err)
	}
	return img, body, nil
}

// analyzeCompanySite fetches a company website and returns name, best
// decodable logo and brand color (theme-color > logo color > navy).
func analyzeCompanySite(ctx context.Context, siteURL string) (SiteMeta, error) {
	if !strings.Contains(siteURL, "://") {
		siteURL = "https://" + siteURL
	}
	body, finalURL, _, err := safeFetch(ctx, siteURL)
	if err != nil {
		return SiteMeta{}, err
	}
	meta := extractMetadata(body, finalURL)
	meta.LogoURL = ""
	var logo image.Image
	for i, cand := range meta.LogoCandidates {
		if i >= 6 {
			break
		}
		img, _, err := fetchImage(ctx, cand)
		if err != nil {
			continue
		}
		if b := img.Bounds(); b.Dx() < 32 || b.Dy() < 32 {
			continue
		}
		meta.LogoURL, logo = cand, img
		break
	}
	meta.BrandColor = defaultBrandColor
	if c, ok := parseColor(meta.ThemeColor); ok && usableBrandColor(c) {
		meta.BrandColor = meta.ThemeColor
	} else if logo != nil {
		if c, ok := dominantColor(logo); ok {
			meta.BrandColor = fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
		}
	}
	return meta, nil
}
