//go:build linux

package agent

// Go port of config/native/release_manifest.py (R5a): verify a signed release manifest before any staged
// payload file is used. Same trust model: the only anchor is an ECDSA P-256 public key supplied by the caller;
// no key ships here; a valid signature is necessary but not sufficient (publisher, version, validity window,
// revocation and the exact size and SHA-256 of every given file must match). Signature format: DER ECDSA over
// SHA-256 of the exact manifest bytes (what `openssl dgst -sha256 -sign` writes). Reasons are the same fixed
// strings as the Python verifier; manifest content is never echoed.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ReleaseSchema = "thingspanel.release-manifest.v1"

var (
	hex64Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ReleaseError carries one of the verifier's fixed reasons (trust_root_not_configured, bad_signature,
// malformed_manifest, wrong_publisher, version_mismatch, revoked, not_yet_valid, expired, bad_path,
// unlisted_file, missing_file, digest_mismatch).
type ReleaseError struct{ Reason string }

func (e *ReleaseError) Error() string { return e.Reason }

func relErr(reason string) error { return &ReleaseError{Reason: reason} }

// ReleasePolicy is what the caller expects of the release. A nil policy means "unsigned mode".
type ReleasePolicy struct {
	TrustRoot string   // path of the pinned PEM public key
	Publisher string   // expected release.publisher
	Revoked   []string // versions withdrawn out of band
	Now       int64    // 0 means the current time (tests inject it)
}

// LoadTrustRoot reads the pinned key; every problem fails closed as trust_root_not_configured.
func LoadTrustRoot(path string) (*ecdsa.PublicKey, error) {
	if path == "" {
		return nil, relErr("trust_root_not_configured")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, relErr("trust_root_not_configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, relErr("trust_root_not_configured")
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, relErr("trust_root_not_configured")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, relErr("trust_root_not_configured")
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, relErr("trust_root_not_configured")
	}
	if _, err := ecdh.P256().NewPublicKey(elliptic.Marshal(key.Curve, key.X, key.Y)); err != nil {
		return nil, relErr("trust_root_not_configured") // not a point on the curve
	}
	return key, nil
}

type releaseEntry struct {
	SHA256 string
	Size   int64
}

type manifestRelease struct {
	Version, Publisher string
	NotBefore, Expires int64
	revokedInside      []string
}

func jsonInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil && !strings.ContainsAny(n.String(), ".eE")
}

func parseManifest(raw []byte) (manifestRelease, map[string]releaseEntry, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return manifestRelease{}, nil, relErr("malformed_manifest")
	}
	if _, err := dec.Token(); err != io.EOF {
		return manifestRelease{}, nil, relErr("malformed_manifest")
	}
	if doc["schema"] != ReleaseSchema {
		return manifestRelease{}, nil, relErr("malformed_manifest")
	}
	release, _ := doc["release"].(map[string]any)
	files, _ := doc["files"].([]any)
	if release == nil || len(files) == 0 {
		return manifestRelease{}, nil, relErr("malformed_manifest")
	}
	var r manifestRelease
	var ok bool
	if r.Version, ok = release["version"].(string); !ok || r.Version == "" {
		return r, nil, relErr("malformed_manifest")
	}
	if r.Publisher, ok = release["publisher"].(string); !ok || r.Publisher == "" {
		return r, nil, relErr("malformed_manifest")
	}
	if c, ok := release["commit"].(string); !ok || !commitPattern.MatchString(c) {
		return r, nil, relErr("malformed_manifest")
	}
	if r.NotBefore, ok = jsonInt(release["not_before"]); !ok {
		return r, nil, relErr("malformed_manifest")
	}
	if r.Expires, ok = jsonInt(release["expires_at"]); !ok {
		return r, nil, relErr("malformed_manifest")
	}
	if list, present := doc["revoked_versions"]; present && list != nil {
		items, ok := list.([]any)
		if !ok {
			return r, nil, relErr("malformed_manifest")
		}
		for _, it := range items {
			if s, ok := it.(string); ok {
				r.revokedInside = append(r.revokedInside, s)
			}
		}
	}
	expected := map[string]releaseEntry{}
	for _, item := range files {
		entry, ok := item.(map[string]any)
		if !ok {
			return r, nil, relErr("malformed_manifest")
		}
		rel, err := cleanReleasePath(entry["path"])
		if err != nil {
			return r, nil, err
		}
		digest, ok1 := entry["sha256"].(string)
		size, ok2 := jsonInt(entry["size"])
		if !ok1 || !hex64Pattern.MatchString(digest) || !ok2 || size < 0 {
			return r, nil, relErr("malformed_manifest")
		}
		if _, dup := expected[rel]; dup {
			return r, nil, relErr("malformed_manifest")
		}
		expected[rel] = releaseEntry{SHA256: digest, Size: size}
	}
	return r, expected, nil
}

func cleanReleasePath(raw any) (string, error) {
	s, ok := raw.(string)
	if !ok || s == "" || strings.ContainsAny(s, "\\\x00") || strings.HasPrefix(s, "/") {
		return "", relErr("bad_path")
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." {
			return "", relErr("bad_path")
		}
	}
	return s, nil
}

// VerifyFiles checks a subset of a release: the signature, identity and validity window of the manifest, and
// that every given file (manifest path -> local copy) is listed with matching size and SHA-256. A file the
// manifest does not list, a symlink or a missing file is rejected; listed files that are not given are not
// checked (this is what an updater's staged files need). It returns the release version.
func VerifyFiles(manifest, signature []byte, policy ReleasePolicy, expectedVersion string, files map[string]string) (string, error) {
	key, err := LoadTrustRoot(policy.TrustRoot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(manifest)
	if !ecdsa.VerifyASN1(key, sum[:], signature) {
		return "", relErr("bad_signature")
	}
	rel, expected, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	if rel.Publisher != policy.Publisher {
		return "", relErr("wrong_publisher")
	}
	if expectedVersion != "" && rel.Version != expectedVersion {
		return "", relErr("version_mismatch")
	}
	for _, v := range append(append([]string{}, policy.Revoked...), rel.revokedInside...) {
		if v == rel.Version {
			return "", relErr("revoked")
		}
	}
	now := policy.Now
	if now == 0 {
		now = time.Now().Unix()
	}
	if now < rel.NotBefore {
		return "", relErr("not_yet_valid")
	}
	if now >= rel.Expires {
		return "", relErr("expired")
	}
	if len(files) == 0 {
		return "", relErr("malformed_manifest")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, err := cleanReleasePath(n); err != nil {
			return "", err
		}
		entry, listed := expected[n]
		if !listed {
			return "", relErr("unlisted_file")
		}
		info, err := os.Lstat(files[n])
		if err != nil || !info.Mode().IsRegular() {
			return "", relErr("missing_file")
		}
		fp, err := fileFingerprint(files[n])
		if err != nil || fp == nil {
			return "", relErr("missing_file")
		}
		if fp.Size != entry.Size || fp.SHA256 != entry.SHA256 {
			return "", relErr("digest_mismatch")
		}
	}
	return rel.Version, nil
}

// manifestDigest is used by tests and tooling that need the digest of a payload file.
func manifestDigest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
