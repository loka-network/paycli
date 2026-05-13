package sdk

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// LokaLndReleaseRepo is the GitHub repo paycli pulls lnd binaries from.
// Kept as a constant so callers (and tests) can swap to a fork without
// touching this file's logic.
const LokaLndReleaseRepo = "loka-network/loka-p2p-lnd"

// DefaultLndVersion is the version paycli installs when no --version is
// passed. Bumped per validated upstream release; older versions stay
// installable explicitly. Picked over "latest" so a paycli release pins
// against a known-good lnd build instead of silently riding the head of
// upstream.
const DefaultLndVersion = "v0.21.0"

// LokaSeedNodes are the official Loka Lightning Network seed nodes
// (per loka-agentic-payment SKILL.md). Connecting to at least one is the
// recommended bootstrap path for new agents.
var LokaSeedNodes = []string{
	"0276bf6dc8fd0ce046c40c0c504d586419ecfdc456909b7f17e60e4da824e7afc7@lnd-seed-eu.loka.cash:9735",
	"0268e7d59cfe59230ac6d0af4750bc5042bd6209e9cae1da32f98f8ee9ef9596a9@lnd-seed-us.loka.cash:9735",
}

// SuiNetwork is "devnet" / "testnet" / "mainnet". paycli currently
// only auto-pins package IDs for the first two — mainnet support is a
// matter of resolving its deploy_state JSON in the same way once the
// repo publishes one.
type SuiNetwork string

const (
	NetworkDevnet  SuiNetwork = "devnet"
	NetworkTestnet SuiNetwork = "testnet"
)

// SuiNetworkConfig is the per-network bundle needed to bring up lnd
// against a Sui chain backend.
type SuiNetworkConfig struct {
	Network   SuiNetwork
	RPCHost   string // passed via --suinode.rpchost
	FaucetURL string // POST {FixedAmountRequest:{recipient:<addr>}}; empty on mainnet
}

// SuiNetworkConfigs is the source of truth for per-network defaults.
// Pulled out so callers don't sprinkle URLs throughout the codebase.
var SuiNetworkConfigs = map[SuiNetwork]SuiNetworkConfig{
	NetworkDevnet: {
		Network:   NetworkDevnet,
		RPCHost:   "https://fullnode.devnet.sui.io:443",
		FaucetURL: "https://faucet.devnet.sui.io/v2/gas",
	},
	NetworkTestnet: {
		Network:   NetworkTestnet,
		RPCHost:   "https://fullnode.testnet.sui.io:443",
		FaucetURL: "https://faucet.testnet.sui.io/v2/gas",
	},
}

// LndInstallResult is what DownloadAndExtractLnd hands back to callers
// so they can persist the binary paths in paycli config.
type LndInstallResult struct {
	Version   string
	BinDir    string // e.g. ~/.paycli/lnd/v0.21.0/bin
	LndPath   string // BinDir + "/lnd"  (".exe" on windows)
	LncliPath string // BinDir + "/lncli"
}

// DownloadAndExtractLnd downloads the loka-lnd release for the running
// host's OS/arch into destRoot/<version>/bin and returns the resolved
// binary paths. If force=false and the binaries already exist for that
// version, it short-circuits and returns the existing paths.
//
// destRoot is typically ~/.paycli/lnd. version may be either a plain
// "v0.21.0" or "latest" (which calls the GitHub API to resolve).
//
// progress, if non-nil, receives one-line status updates (download
// start, byte counts during transfer, extract complete). Pass
// os.Stderr from a CLI; nil to silence.
func DownloadAndExtractLnd(ctx context.Context, destRoot, version string, force bool, progress io.Writer) (*LndInstallResult, error) {
	if version == "" || version == "latest" {
		v, err := resolveLatestLndVersion(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve latest version: %w", err)
		}
		version = v
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	binDir := filepath.Join(destRoot, version, "bin")
	lndName, lncliName := "lnd", "lncli"
	if runtime.GOOS == "windows" {
		lndName, lncliName = "lnd.exe", "lncli.exe"
	}
	res := &LndInstallResult{
		Version:   version,
		BinDir:    binDir,
		LndPath:   filepath.Join(binDir, lndName),
		LncliPath: filepath.Join(binDir, lncliName),
	}

	if !force && fileExists(res.LndPath) && fileExists(res.LncliPath) {
		logProgress(progress, "lnd %s already installed at %s — skipping download (pass --force to redownload)", version, binDir)
		return res, nil
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", binDir, err)
	}

	assetName, err := releaseAssetName(version)
	if err != nil {
		return nil, err
	}
	assetURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", LokaLndReleaseRepo, version, assetName)

	logProgress(progress, "downloading %s …", assetURL)
	body, size, err := httpGetStream(ctx, assetURL)
	if err != nil {
		return nil, fmt.Errorf("download asset: %w", err)
	}
	defer body.Close()

	// archive name → file extension dispatch.
	wantNames := map[string]string{
		lndName:   res.LndPath,
		lncliName: res.LncliPath,
	}
	if strings.HasSuffix(assetName, ".zip") {
		if err := extractZipMembersStream(body, size, wantNames, progress); err != nil {
			return nil, fmt.Errorf("extract zip: %w", err)
		}
	} else {
		if err := extractTarGzMembers(body, wantNames, progress); err != nil {
			return nil, fmt.Errorf("extract tar.gz: %w", err)
		}
	}

	// Mark binaries executable (no-op on windows).
	if runtime.GOOS != "windows" {
		for _, p := range []string{res.LndPath, res.LncliPath} {
			if err := os.Chmod(p, 0o755); err != nil {
				return nil, fmt.Errorf("chmod %s: %w", p, err)
			}
		}
	}
	if !fileExists(res.LndPath) || !fileExists(res.LncliPath) {
		return nil, fmt.Errorf("archive %s did not contain expected binaries %s + %s", assetName, lndName, lncliName)
	}
	logProgress(progress, "installed %s → %s", version, binDir)
	return res, nil
}

// FetchSuiPackageID returns the deployed Lightning Move package ID for
// the given network, fetched from the loka-p2p-lnd repo at the same
// version tag as the binary. The schema is
//
//	{ "package_id": "0x…", "upgrade_cap": "0x…" }
//
// per sui-contracts/lightning/deploy_state_<network>.json.
//
// version may be "v0.21.0" / "main" / a branch name; "" defaults to
// DefaultLndVersion.
func FetchSuiPackageID(ctx context.Context, network SuiNetwork, version string) (string, error) {
	if version == "" {
		version = DefaultLndVersion
	}
	url := fmt.Sprintf(
		"https://raw.githubusercontent.com/%s/%s/sui-contracts/lightning/deploy_state_%s.json",
		LokaLndReleaseRepo, version, network,
	)
	body, _, err := httpGetStream(ctx, url)
	if err != nil {
		return "", fmt.Errorf("fetch deploy_state_%s.json: %w", network, err)
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 16*1024))
	if err != nil {
		return "", err
	}
	var ds struct {
		PackageID string `json:"package_id"`
	}
	if err := json.Unmarshal(raw, &ds); err != nil {
		return "", fmt.Errorf("decode deploy_state json: %w (body=%q)", err, string(raw))
	}
	if ds.PackageID == "" {
		return "", fmt.Errorf("deploy_state_%s.json missing package_id (body=%q)", network, string(raw))
	}
	return ds.PackageID, nil
}

// releaseAssetName composes the platform-specific archive name following
// loka-p2p-lnd's release convention:
//
//	loka-lnd-<os>-<arch>-<version>.tar.gz  (.zip on windows)
//
// Returns an error for arch/os combinations the upstream doesn't ship.
func releaseAssetName(version string) (string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	supported := map[string]map[string]bool{
		"darwin":  {"amd64": true, "arm64": true},
		"linux":   {"amd64": true, "arm64": true, "386": true, "arm": true}, // arm → armv7 below
		"windows": {"amd64": true, "386": true, "arm": true},
		"freebsd": {"amd64": true, "386": true, "arm": true},
		"netbsd":  {"amd64": true},
		"openbsd": {"amd64": true},
	}
	if _, ok := supported[osName]; !ok {
		return "", fmt.Errorf("unsupported os %q (upstream ships: darwin linux windows freebsd netbsd openbsd)", osName)
	}
	if !supported[osName][arch] {
		return "", fmt.Errorf("unsupported arch %q for %s — upstream loka-p2p-lnd does not ship this combo", arch, osName)
	}
	// Go's runtime.GOARCH "arm" is generic; upstream publishes armv6 and
	// armv7. armv7 covers Raspberry Pi 2+ and most server boards; pick it
	// as the default until we add a GOARM-aware probe.
	if arch == "arm" {
		arch = "armv7"
	}
	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("loka-lnd-%s-%s-%s.%s", osName, arch, version, ext), nil
}

func resolveLatestLndVersion(ctx context.Context) (string, error) {
	// GitHub's redirect-based "latest" endpoint avoids API rate limits for
	// unauthenticated clients (the JSON /releases/latest is rate-limited;
	// /releases/latest as an HTML page is a 302 we can sniff).
	url := fmt.Sprintf("https://github.com/%s/releases/latest", LokaLndReleaseRepo)
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no redirect from /releases/latest (status %d)", resp.StatusCode)
	}
	// /releases/tag/v0.21.0  →  v0.21.0
	idx := strings.LastIndex(loc, "/")
	if idx < 0 || idx == len(loc)-1 {
		return "", fmt.Errorf("unexpected Location: %s", loc)
	}
	return loc[idx+1:], nil
}

// httpGetStream returns the response body + Content-Length (or -1 if
// unknown). Caller closes the body.
func httpGetStream(ctx context.Context, url string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "paycli")
	client := &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %s: status %d (%s)", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, resp.ContentLength, nil
}

// extractTarGzMembers walks the tar.gz stream once, writing any file
// whose basename matches a key in wantNames to the corresponding path.
// Files not in the map are skipped — we never extract the whole release.
func extractTarGzMembers(r io.Reader, wantNames map[string]string, progress io.Writer) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		base := filepath.Base(hdr.Name)
		dst, ok := wantNames[base]
		if !ok {
			continue
		}
		logProgress(progress, "  → extracting %s (%d bytes)", base, hdr.Size)
		if err := writeStreamToFile(tr, dst); err != nil {
			return err
		}
	}
}

// extractZipMembersStream buffers the response (zip needs random access)
// to a temp file, then walks it. size is the Content-Length or -1; we
// pass it to TempFile via a Preallocate-style hint, but in practice we
// just stream-copy regardless.
func extractZipMembersStream(r io.Reader, _ int64, wantNames map[string]string, progress io.Writer) error {
	tmp, err := os.CreateTemp("", "paycli-lnd-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	fi, err := tmp.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(tmp, fi.Size())
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		dst, ok := wantNames[filepath.Base(f.Name)]
		if !ok {
			continue
		}
		logProgress(progress, "  → extracting %s (%d bytes)", filepath.Base(f.Name), f.UncompressedSize64)
		src, err := f.Open()
		if err != nil {
			return err
		}
		err = writeStreamToFile(src, dst)
		src.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeStreamToFile(src io.Reader, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func logProgress(w io.Writer, format string, args ...interface{}) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}
