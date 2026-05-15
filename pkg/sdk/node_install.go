package sdk

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

// LokaLndReleaseRepo is the GitHub repo lokapay pulls lnd binaries from.
// Kept as a constant so callers (and tests) can swap to a fork without
// touching this file's logic.
const LokaLndReleaseRepo = "loka-network/loka-p2p-lnd"

// DefaultLndVersion is the version lokapay installs when no --version is
// passed. Bumped per validated upstream release; older versions stay
// installable explicitly. Picked over "latest" so a lokapay release pins
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

// SuiNetwork picks which Sui chain lnd talks to. devnet / testnet /
// mainnet are wired against upstream Sui fullnodes — the Move package
// id is resolved at runtime from sui-contracts/lightning/deploy_state_<network>.json
// in the loka-p2p-lnd repo, so mainnet works as soon as upstream publishes
// deploy_state_mainnet.json. localnet is for developer machines running
// `sui start --with-faucet --force-regenesis`; package id has to be
// supplied by hand since each regenesis re-publishes the Move package.
type SuiNetwork string

const (
	NetworkDevnet   SuiNetwork = "devnet"
	NetworkTestnet  SuiNetwork = "testnet"
	NetworkMainnet  SuiNetwork = "mainnet"
	NetworkLocalnet SuiNetwork = "localnet"
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
	NetworkMainnet: {
		Network:   NetworkMainnet,
		RPCHost:   "https://fullnode.mainnet.sui.io:443",
		FaucetURL: "", // no faucet on mainnet; users must fund the address themselves
	},
	NetworkLocalnet: {
		Network:   NetworkLocalnet,
		RPCHost:   "http://127.0.0.1:9000",
		FaucetURL: "http://127.0.0.1:9123/v2/gas",
	},
}

// LndSuiNetworkFlag returns the --suinode.<x> flag name lnd accepts for
// the given Sui network. lnd ships with mainnet / testnet / devnet /
// simnet — there is *no* `--suinode.localnet`. By convention (matching
// itest_sui_single_coin.sh) localnet runs use the devnet flag plus a
// rpchost pointing at 127.0.0.1:9000; the on-disk chain dir name lnd
// chooses is "devnet" too.
func LndSuiNetworkFlag(n SuiNetwork) SuiNetwork {
	if n == NetworkLocalnet {
		return NetworkDevnet
	}
	return n
}

// LocalnetItestPeer describes one of the lnd nodes that
// scripts/itest_sui_single_coin.sh spins up on a developer machine.
// lokapay uses this to (a) connect to those peers and (b) open a small
// outbound channel — the peer's pubkey is regenerated on every itest
// run, so it has to be queried at connect-time via the lnd REST API.
type LocalnetItestPeer struct {
	Name       string // "alice" or "bob"
	RESTAddr   string // "https://127.0.0.1:8081"
	P2PAddr    string // "127.0.0.1:10011" — host:port the peer's lnd listens on
	LndDataDir string // "/tmp/lnd-sui-test/alice"
}

// LocalnetItestPeers is the canonical list, in the same order itest opens
// channels (Bob → Alice first, then Alice → Bob). lokapay defaults to
// channel-opening against the first entry (alice) which already has a
// channel back to bob, so a single channel to alice gives lokapay
// full alice ↔ bob routing.
var LocalnetItestPeers = []LocalnetItestPeer{
	{
		Name:       "alice",
		RESTAddr:   "https://127.0.0.1:8081",
		P2PAddr:    "127.0.0.1:10011",
		LndDataDir: "/tmp/lnd-sui-test/alice",
	},
	{
		Name:       "bob",
		RESTAddr:   "https://127.0.0.1:8082",
		P2PAddr:    "127.0.0.1:10012",
		LndDataDir: "/tmp/lnd-sui-test/bob",
	},
}

// MacaroonPath returns the admin.macaroon path for a peer on the given
// Sui network. lnd writes chain data under data/chain/sui/<network>
// where <network> is whatever `--suinode.<x>` flag was used at start;
// localnet maps to "devnet" (see LndSuiNetworkFlag) so the on-disk
// dir is `.../sui/devnet/admin.macaroon`.
func (p LocalnetItestPeer) MacaroonPath(network SuiNetwork) string {
	return filepath.Join(p.LndDataDir, "data", "chain", "sui", string(LndSuiNetworkFlag(network)), "admin.macaroon")
}

// lndGetInfoResult is the subset of /v1/getinfo lokapay reads.
type lndGetInfoResult struct {
	IdentityPubkey string `json:"identity_pubkey"`
	Alias          string `json:"alias"`
}

// FetchLndIdentityPubkey calls /v1/getinfo on an lnd REST endpoint and
// returns its identity_pubkey. macaroonPath must point at admin.macaroon
// for that node. TLS verification is skipped — lnd uses a self-signed
// cert and lokapay already treats the local managed lnd the same way.
func FetchLndIdentityPubkey(ctx context.Context, restAddr, macaroonPath string) (string, error) {
	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return "", fmt.Errorf("read macaroon %s: %w", macaroonPath, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(restAddr, "/")+"/v1/getinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Grpc-Metadata-macaroon", hex.EncodeToString(macBytes))
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed lnd cert
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("getinfo http %d: %s", resp.StatusCode, string(body))
	}
	var info lndGetInfoResult
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode getinfo: %w", err)
	}
	if info.IdentityPubkey == "" {
		return "", fmt.Errorf("getinfo returned empty identity_pubkey")
	}
	return info.IdentityPubkey, nil
}

// LndInstallResult is what DownloadAndExtractLnd hands back to callers
// so they can persist the binary paths in lokapay config.
type LndInstallResult struct {
	Version   string
	BinDir    string // e.g. ~/.lokapay/lnd/v0.21.0/bin
	LndPath   string // BinDir + "/lnd"  (".exe" on windows)
	LncliPath string // BinDir + "/lncli"
}

// DownloadAndExtractLnd downloads the loka-lnd release for the running
// host's OS/arch into destRoot/<version>/bin and returns the resolved
// binary paths. If force=false and the binaries already exist for that
// version, it short-circuits and returns the existing paths.
//
// destRoot is typically ~/.lokapay/lnd. version may be either a plain
// "v0.21.0" or "latest" (which calls the GitHub API to resolve).
//
// progress, if non-nil, receives one-line status updates (download
// start, byte counts during transfer, extract complete). Pass
// os.Stderr from a CLI; nil to silence.
func DownloadAndExtractLnd(ctx context.Context, destRoot, version string, force bool, progress io.Writer) (*LndInstallResult, error) {
	// Dev-time escape hatch: LOKAPAY_LND_BINARY_DIR points at a
	// directory containing locally-built `lnd` + `lncli` (e.g. the
	// loka-p2p-lnd repo's repo-root). Skip the download entirely and
	// pretend they came from a release. Useful when iterating on
	// lnd-sui fixes (Move contract / commit-sig sort / etc.) before
	// cutting a new release tag.
	if local := strings.TrimSpace(os.Getenv("LOKAPAY_LND_BINARY_DIR")); local != "" {
		lndName, lncliName := "lnd", "lncli"
		if runtime.GOOS == "windows" {
			lndName, lncliName = "lnd.exe", "lncli.exe"
		}
		res := &LndInstallResult{
			Version:   version,
			BinDir:    local,
			LndPath:   filepath.Join(local, lndName),
			LncliPath: filepath.Join(local, lncliName),
		}
		// Tolerate the loka-p2p-lnd repo's debug build names too.
		if !fileExists(res.LndPath) {
			if alt := filepath.Join(local, "lnd-debug"); fileExists(alt) {
				res.LndPath = alt
			}
		}
		if !fileExists(res.LncliPath) {
			if alt := filepath.Join(local, "lncli-debug"); fileExists(alt) {
				res.LncliPath = alt
			}
		}
		if !fileExists(res.LndPath) || !fileExists(res.LncliPath) {
			return nil, fmt.Errorf("LOKAPAY_LND_BINARY_DIR=%s did not contain lnd + lncli (also tried lnd-debug / lncli-debug)", local)
		}
		logProgress(progress, "using local lnd from $LOKAPAY_LND_BINARY_DIR: %s", local)
		return res, nil
	}
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

	logProgress(progress, "downloading %s", assetURL)

	// Two-phase: stream to a `.part` file with retry first, then extract
	// from disk. Previously a transient connection cut from GitHub's CDN
	// midway through the body would surface as `extract tar.gz: unexpected
	// EOF` with no automatic recovery, because the body was piped straight
	// into the tar reader. Buffering to disk also lets us print a real
	// progress bar and verify Content-Length before extraction.
	tmpPath := filepath.Join(destRoot, version, assetName+".part")
	defer os.Remove(tmpPath)
	const downloadRetries = 3
	var lastErr error
	for attempt := 1; attempt <= downloadRetries; attempt++ {
		if attempt > 1 {
			logProgress(progress, "retry %d/%d after: %v", attempt, downloadRetries, lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if err := downloadToFile(ctx, assetURL, tmpPath, progress); err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, fmt.Errorf("download asset after %d attempts: %w", downloadRetries, lastErr)
	}

	wantNames := map[string]string{
		lndName:   res.LndPath,
		lncliName: res.LncliPath,
	}
	archive, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open downloaded archive: %w", err)
	}
	defer archive.Close()
	if strings.HasSuffix(assetName, ".zip") {
		info, err := archive.Stat()
		if err != nil {
			return nil, err
		}
		if err := extractZipMembersStream(archive, info.Size(), wantNames, progress); err != nil {
			return nil, fmt.Errorf("extract zip: %w", err)
		}
	} else {
		if err := extractTarGzMembers(archive, wantNames, progress); err != nil {
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

// downloadToFile streams the HTTP body at url into dstPath, drawing a
// live progress bar to progress (when it's a TTY) and validating that
// the byte count matches the response's Content-Length. Returns an
// error suitable for a retry loop on truncation / network blip.
func downloadToFile(ctx context.Context, url, dstPath string, progress io.Writer) error {
	body, total, err := httpGetStream(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()

	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	pr := newProgressReader(body, total, progress, "lnd")
	n, copyErr := io.Copy(out, pr)
	closeErr := out.Close()
	pr.finish()
	if copyErr != nil {
		return fmt.Errorf("body copy after %d bytes: %w", n, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if total > 0 && n != total {
		return fmt.Errorf("incomplete download: got %d of %d bytes", n, total)
	}
	return nil
}

// progressReader wraps an io.Reader and renders byte-count + bar + speed
// to its writer as the underlying reader is consumed. On a TTY it draws
// a single self-overwriting line via '\r'; off-TTY it emits one line
// every ~1s so logs stay readable. nil writer disables progress
// rendering entirely.
type progressReader struct {
	r        io.Reader
	total    int64 // -1 when unknown
	read     int64
	w        io.Writer
	isTTY    bool
	started  time.Time
	lastDraw time.Time
	label    string
}

func newProgressReader(r io.Reader, total int64, w io.Writer, label string) *progressReader {
	return &progressReader{
		r:       r,
		total:   total,
		w:       w,
		isTTY:   isTTYWriter(w),
		started: time.Now(),
		label:   label,
	}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		interval := 100 * time.Millisecond
		if !p.isTTY {
			interval = time.Second
		}
		now := time.Now()
		if now.Sub(p.lastDraw) >= interval {
			p.draw(now)
			p.lastDraw = now
		}
	}
	return n, err
}

func (p *progressReader) finish() {
	if p.w == nil {
		return
	}
	p.draw(time.Now())
	if p.isTTY {
		fmt.Fprintln(p.w)
	}
}

func (p *progressReader) draw(now time.Time) {
	if p.w == nil {
		return
	}
	elapsed := now.Sub(p.started)
	rate := "—"
	if elapsed.Seconds() > 0.05 {
		rate = humanBytes(int64(float64(p.read)/elapsed.Seconds())) + "/s"
	}

	// Non-TTY path: a single line per draw, no escape codes. Multi-line in
	// the log is OK here — it's a log, not a screen.
	if !p.isTTY {
		if p.total > 0 {
			pct := float64(p.read) * 100 / float64(p.total)
			fmt.Fprintf(p.w, "%s %5.1f%% (%s / %s, %s)\n",
				p.label, pct, humanBytes(p.read), humanBytes(p.total), rate)
		} else {
			fmt.Fprintf(p.w, "%s %s (%s)\n", p.label, humanBytes(p.read), rate)
		}
		return
	}

	// TTY path: one self-overwriting line. We must fit inside the
	// terminal width or `\r` only resets to the start of the wrapped
	// physical row and the user sees a multi-line stair-step. Compute
	// the bar width from whatever space is left after the fixed parts.
	cols := termCols(p.w)
	if p.total > 0 {
		pct := float64(p.read) * 100 / float64(p.total)
		// Fixed-width tail = " 100.0%  XXX.XX MB / XXX.XX MB  XXX.X KB/s"
		// Bar bracketed with spaces. Compute headroom.
		tail := fmt.Sprintf("%5.1f%%  %s / %s  %s", pct, humanBytes(p.read), humanBytes(p.total), rate)
		// 5 = label-gap + " [" + "] " spacing
		barW := cols - len(p.label) - len(tail) - 5
		if barW < 6 {
			// Too narrow — drop the bar.
			fmt.Fprintf(p.w, "\r\033[K%s  %s", p.label, tail)
			return
		}
		if barW > 30 {
			barW = 30
		}
		filled := int(pct / 100 * float64(barW))
		if filled < 0 {
			filled = 0
		}
		if filled > barW {
			filled = barW
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
		fmt.Fprintf(p.w, "\r\033[K%s [%s] %s", p.label, bar, tail)
		return
	}
	fmt.Fprintf(p.w, "\r\033[K%s  %s  %s", p.label, humanBytes(p.read), rate)
}

// termCols returns the terminal width of a TTY writer, or 80 when it
// can't be queried. Used to fit the progress bar so `\r` keeps
// overwriting the same physical row.
func termCols(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if c, _, err := term.GetSize(int(f.Fd())); err == nil && c > 20 {
			return c
		}
	}
	return 80
}

func humanBytes(n int64) string {
	const u = 1024.0
	f := float64(n)
	switch {
	case n >= int64(u*u*u):
		return fmt.Sprintf("%.2f GB", f/(u*u*u))
	case n >= int64(u*u):
		return fmt.Sprintf("%.2f MB", f/(u*u))
	case n >= int64(u):
		return fmt.Sprintf("%.1f KB", f/u)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func isTTYWriter(w io.Writer) bool {
	if w == nil {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// httpGetStream returns the response body + Content-Length (or -1 if
// unknown). Caller closes the body.
func httpGetStream(ctx context.Context, url string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "lokapay")
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
