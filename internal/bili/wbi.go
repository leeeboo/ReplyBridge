package bili

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

var wbiMixinKeyEncTab = [...]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52,
}

// WBIKeys are the image and sub keys obtained from x/web-interface/nav.
type WBIKeys struct{ ImgKey, SubKey string }

func (k WBIKeys) Valid() bool { return k.ImgKey != "" && k.SubKey != "" }

// WBIKeyFromURL extracts the key portion of a Bilibili WBI image URL.
func WBIKeyFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	name := path.Base(u.Path)
	return strings.TrimSuffix(name, path.Ext(name))
}

// MixinKey implements Bilibili's published WBI permutation and truncation.
func MixinKey(imgKey, subKey string) string {
	original := imgKey + subKey
	var b strings.Builder
	b.Grow(32)
	for _, i := range wbiMixinKeyEncTab {
		if i < len(original) {
			b.WriteByte(original[i])
		}
	}
	mixed := b.String()
	if len(mixed) > 32 {
		return mixed[:32]
	}
	return mixed
}

// SignWBI adds wts and w_rid to a copy of params. Values are sorted, WBI's
// five disallowed characters are removed, and spaces are encoded as %20.
func SignWBI(params url.Values, keys WBIKeys, now time.Time) url.Values {
	signed := make(url.Values, len(params)+2)
	for key, values := range params {
		for _, value := range values {
			signed.Add(key, stripWBIChars(value))
		}
	}
	signed.Set("wts", strconv.FormatInt(now.Unix(), 10))
	canonical := encodeWBI(signed)
	hash := md5.Sum([]byte(canonical + MixinKey(keys.ImgKey, keys.SubKey)))
	signed.Set("w_rid", hex.EncodeToString(hash[:]))
	return signed
}

// EncodeWBI is exported for deterministic tests and diagnostics; it uses the
// exact percent encoding required by the signature rather than '+' for space.
func EncodeWBI(values url.Values) string { return encodeWBI(values) }

func encodeWBI(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		vals := values[key]
		if len(vals) == 0 {
			parts = append(parts, queryEscapeWBI(key)+"=")
			continue
		}
		for _, value := range vals {
			parts = append(parts, queryEscapeWBI(key)+"="+queryEscapeWBI(value))
		}
	}
	return strings.Join(parts, "&")
}

func queryEscapeWBI(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

func stripWBIChars(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '!', '\'', '(', ')', '*':
			return -1
		}
		return r
	}, s)
}
