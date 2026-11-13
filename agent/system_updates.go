//go:build linux

package agent

// "Check for updates" (R6): read the signed release manifest the upgrade source serves, verify it exactly as an
// upgrade would (signature, publisher, validity window, revocation) and compare its version with the running one.
// Nothing is installed or changed; the answer carries no secrets and no manifest content beyond the version and
// the validity window.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codes of a failed check.
const (
	CodeUpdateNoSource  = "updates_not_configured"
	CodeUpdateFetch     = "updates_fetch_failed"
	CodeUpdateRejected  = "updates_manifest_rejected"
	CodeUpdateNoVersion = "updates_current_version_unknown"
	releaseManifestName = "release-manifest.json"
)

// UpdateInfo is the answer to "check for updates".
type UpdateInfo struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest"`
	Newer     bool      `json:"newer"` // the served release is newer than the running version
	ValidFrom time.Time `json:"valid_from"`
	ValidTo   time.Time `json:"valid_until"`
	Reason    string    `json:"reason,omitempty"` // when the release is served but not usable (e.g. expired)
}

// UpdateChecker answers update queries for a configured release source.
type UpdateChecker struct {
	Config   UpgradeConfig
	Download Downloader
	Current  func(ctx context.Context) (string, bool)
	Now      func() time.Time
}

// Check fetches and verifies the manifest. The error is always a CodedError.
func (u *UpdateChecker) Check(ctx context.Context) (UpdateInfo, error) {
	cfg := u.Config
	if cfg.Source == "" {
		return UpdateInfo{}, CodedError{CodeUpdateNoSource}
	}
	dir, err := os.MkdirTemp("", "tp-updates-")
	if err != nil {
		return UpdateInfo{}, CodedError{CodeUpdateFetch}
	}
	defer os.RemoveAll(dir)
	base := strings.TrimRight(cfg.Source, "/") + "/" + releaseManifestName
	if err := u.Download(base, filepath.Join(dir, "m")); err != nil {
		return UpdateInfo{}, CodedError{CodeUpdateFetch}
	}
	if err := u.Download(base+".sig", filepath.Join(dir, "s")); err != nil {
		return UpdateInfo{}, CodedError{CodeUpdateFetch}
	}
	manifest, err1 := os.ReadFile(filepath.Join(dir, "m"))
	sig, err2 := os.ReadFile(filepath.Join(dir, "s"))
	if err1 != nil || err2 != nil {
		return UpdateInfo{}, CodedError{CodeUpdateFetch}
	}
	publisher := cfg.Publisher
	if publisher == "" {
		publisher = defaultReleasePublisher
	}
	policy := ReleasePolicy{TrustRoot: cfg.TrustRoot, Publisher: publisher, Revoked: cfg.Revoked}
	if u.Now != nil {
		policy.Now = u.Now().Unix()
	}
	rel, err := VerifyManifest(manifest, sig, policy)
	if err != nil {
		return UpdateInfo{}, CodedError{CodeUpdateRejected}
	}
	cur, ok := u.Current(ctx)
	if !ok || cur == "" {
		return UpdateInfo{}, CodedError{CodeUpdateNoVersion}
	}
	info := UpdateInfo{Current: cur, Latest: rel.Version, ValidFrom: time.Unix(rel.NotBefore, 0).UTC(), ValidTo: time.Unix(rel.ExpiresAt, 0).UTC()}
	if cmp, ok := CompareVersions(rel.Version, cur); ok && cmp > 0 {
		info.Newer = true
	}
	return info, nil
}
