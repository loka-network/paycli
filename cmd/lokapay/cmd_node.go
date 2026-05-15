package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdNode is the `lokapay node …` subcommand group: a turn-key wrapper
// for downloading + running a local loka-lnd against the Sui chain
// backend without the operator having to clone the source repo.
//
// All state lives under ~/.lokapay/{lnd,lnd-data}/. The flow follows
// loka-agentic-payment SKILL.md: install binaries → start with the
// right --suinode flags → optionally hit the devnet faucet → optionally
// connect to the Loka seed nodes → register paths in lokapay config so
// every subsequent `lokapay pay / fund / request` dispatches against the
// running node.
func cmdNode() *cli.Command {
	return &cli.Command{
		Name:  "node",
		Usage: "Manage a lokapay-managed local loka-lnd (download, start, stop, status, logs)",
		Subcommands: []*cli.Command{
			cmdNodeInstall(),
			cmdNodeStart(),
			cmdNodeStop(),
			cmdNodeRestart(),
			cmdNodeUpgrade(),
			cmdNodeStatus(),
			cmdNodeLogs(),
			cmdNodeFaucet(),
		},
	}
}

func cmdNodeFaucet() *cli.Command {
	return &cli.Command{
		Name:  "faucet",
		Usage: "Request test SUI for the managed lnd's wallet (devnet / testnet only)",
		Description: "Derives a Sui address via `lncli newaddress p2wkh` and POSTs " +
			"it to the network's faucet (https://faucet.{devnet,testnet}.sui.io/v2/gas). " +
			"Useful when the on-chain balance has been spent down opening channels " +
			"and you need to top up without rerunning the full `node start` bring-up.",
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.Network == "" {
				return fail("node faucet: no managed lnd recorded — run `lokapay node install` + `lokapay node start` first")
			}
			netCfg, ok := sdk.SuiNetworkConfigs[sdk.SuiNetwork(cfg.Node.Network)]
			if !ok {
				return fail("node faucet: unknown saved network %q", cfg.Node.Network)
			}
			if netCfg.FaucetURL == "" {
				return fail("node faucet: no faucet on %s — fund the address manually", cfg.Node.Network)
			}
			return fundWalletFromFaucet(c.Context, cfg, netCfg)
		},
	}
}

func cmdNodeInstall() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "Download the loka-lnd release for this host and remember its path in lokapay config",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "version", Value: sdk.DefaultLndVersion, Usage: "release tag, e.g. v0.21.0 (or 'latest' to resolve via GitHub)"},
			&cli.BoolFlag{Name: "force", Usage: "redownload + overwrite even if binaries already exist at this version"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			destRoot, err := lokapayLndRoot()
			if err != nil {
				return err
			}
			res, err := sdk.DownloadAndExtractLnd(c.Context, destRoot, c.String("version"), c.Bool("force"), os.Stderr)
			if err != nil {
				return fail("node install: %v", err)
			}
			cfg.Node.LndBinaryPath = res.LndPath
			cfg.Node.LncliBinaryPath = res.LncliPath
			cfg.Node.LndVersion = res.Version
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			fmt.Fprintf(os.Stderr, "\n✓ lnd %s installed\n  lnd:    %s\n  lncli:  %s\n", res.Version, res.LndPath, res.LncliPath)
			return nil
		},
	}
}

func cmdNodeStart() *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "Boot the installed loka-lnd against a Sui chain, write its paths into lokapay config",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "network", Value: string(sdk.NetworkDevnet), Usage: "sui chain to connect to: devnet | testnet | mainnet | localnet"},
			&cli.StringFlag{Name: "lnddir", Usage: "lnd data directory (default ~/.lokapay/lnd-data)"},
			&cli.StringFlag{Name: "package-id", Usage: "override the auto-resolved Sui Lightning Move package ID"},
			&cli.IntFlag{Name: "rpc-port", Value: 10009, Usage: "lnd gRPC port"},
			&cli.IntFlag{Name: "rest-port", Value: 8081, Usage: "lnd REST port"},
			&cli.IntFlag{Name: "p2p-port", Value: 9735, Usage: "lnd Lightning P2P port"},
			&cli.BoolFlag{Name: "faucet", Usage: "after start, hit the Sui faucet for the lnd wallet (devnet/testnet/localnet only — no faucet on mainnet)"},
			&cli.BoolFlag{Name: "connect-seeds", Usage: "after start, connect to peers: Loka EU + US seed nodes on devnet/testnet/mainnet, or itest alice/bob on localnet"},
			&cli.DurationFlag{Name: "wait-timeout", Value: 30 * time.Second, Usage: "how long to wait for lnd's RPC port to come up"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.LndBinaryPath == "" {
				return fail("node start: no lnd binary installed — run `lokapay node install` first")
			}
			network := sdk.SuiNetwork(strings.ToLower(c.String("network")))
			netCfg, ok := sdk.SuiNetworkConfigs[network]
			if !ok {
				return fail("node start: unknown --network %q (want devnet | testnet | mainnet | localnet)", network)
			}
			if c.Bool("faucet") && netCfg.FaucetURL == "" {
				fmt.Fprintf(os.Stderr, "warning: --faucet ignored on %s (no faucet endpoint)\n", network)
			}

			lndDir := c.String("lnddir")
			if lndDir == "" {
				if cfg.Node.LndDir != "" {
					lndDir = cfg.Node.LndDir
				} else {
					d, err := lokapayLndDataDir()
					if err != nil {
						return err
					}
					lndDir = d
				}
			}
			if err := os.MkdirAll(lndDir, 0o700); err != nil {
				return fail("mkdir %s: %v", lndDir, err)
			}

			pkgID := c.String("package-id")
			if pkgID == "" {
				fmt.Fprintf(os.Stderr, "→ resolving Sui Lightning package ID for %s …\n", network)
				p, err := sdk.FetchSuiPackageID(c.Context, network, cfg.Node.LndVersion)
				if err != nil {
					return fail("node start: %v (override with --package-id)", err)
				}
				pkgID = p
				fmt.Fprintf(os.Stderr, "  package_id: %s\n", pkgID)
			}

			if alreadyRunning(lndDir) {
				return fail("node start: an lnd process is already recorded in %s/lnd.pid — run `lokapay node stop` first", lndDir)
			}

			ports := managedLndPorts{P2P: c.Int("p2p-port"), RPC: c.Int("rpc-port"), REST: c.Int("rest-port")}
			if err := spawnManagedLnd(c.Context, cfg, lndDir, network, netCfg, pkgID, ports, c.Duration("wait-timeout")); err != nil {
				return fail("node start: %v", err)
			}

			cfg.Node.Endpoint = fmt.Sprintf("https://127.0.0.1:%d", ports.REST)
			cfg.Node.TLSCertPath = filepath.Join(lndDir, "tls.cert")
			cfg.Node.MacaroonPath = filepath.Join(lndDir, "data", "chain", "sui", string(sdk.LndSuiNetworkFlag(network)), "admin.macaroon")
			cfg.Node.LndDir = lndDir
			cfg.Node.Network = string(network)
			cfg.Node.PackageID = pkgID
			cfg.Node.RPCPort = ports.RPC
			cfg.Insecure = true // self-signed lnd cert; same default as the itest
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}

			// Optional bootstrap actions. Each gets a short timeout; failures
			// are warnings, not fatal — the node is up either way.
			if c.Bool("faucet") {
				if err := fundWalletFromFaucet(c.Context, cfg, netCfg); err != nil {
					fmt.Fprintf(os.Stderr, "warning: faucet bootstrap failed: %v\n", err)
				}
			}
			if c.Bool("connect-seeds") {
				connectPeers(c.Context, cfg, network)
			}

			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "Next:")
			fmt.Fprintln(os.Stderr, "  lokapay config set route node")
			fmt.Fprintln(os.Stderr, "  lokapay node status         # confirm pubkey + balance")
			fmt.Fprintln(os.Stderr, "  lokapay node logs -f        # tail the lnd log")
			return nil
		},
	}
}

func cmdNodeStop() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "Stop the lokapay-managed lnd (graceful via lncli, SIGTERM fallback)",
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.LndDir == "" {
				return fail("node stop: no managed lnd recorded in config")
			}
			return stopManagedLnd(c.Context, cfg)
		},
	}
}

// stopManagedLnd asks lnd to shut down, falling back to SIGTERM if
// the graceful path errors. Shared by `lokapay node stop`,
// `lokapay node restart`, and `lokapay node upgrade`.
func stopManagedLnd(ctx context.Context, cfg *Config) error {
	if cfg.Node.LncliBinaryPath != "" && cfg.Node.MacaroonPath != "" {
		out, err := runLncli(ctx, cfg, "stop")
		if err == nil {
			fmt.Fprintf(os.Stderr, "✓ lnd stopped (graceful): %s\n", strings.TrimSpace(out))
			removePidFile(cfg.Node.LndDir)
			return nil
		}
		fmt.Fprintf(os.Stderr, "warning: graceful stop failed (%v); falling back to SIGTERM\n", err)
	}
	pid, err := readPidFile(cfg.Node.LndDir)
	if err != nil {
		return err
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := sendStopSignal(p); err != nil {
		return fmt.Errorf("stop pid %d: %w", pid, err)
	}
	fmt.Fprintf(os.Stderr, "✓ stop signal sent to pid %d\n", pid)
	removePidFile(cfg.Node.LndDir)
	return nil
}

// derivedPorts returns the port triple to pass into spawnManagedLnd
// on a restart/upgrade. Parses REST from cfg.Node.Endpoint and falls
// back to the defaults if anything is missing — same effective values
// as a fresh `lokapay node start` would pick.
func derivedPorts(cfg *Config) managedLndPorts {
	ports := defaultManagedLndPorts
	if cfg.Node.Endpoint == "" {
		return ports
	}
	// Endpoint shape: https://127.0.0.1:<rest>
	if i := strings.LastIndex(cfg.Node.Endpoint, ":"); i > 0 {
		if p, err := strconv.Atoi(strings.TrimSuffix(cfg.Node.Endpoint[i+1:], "/")); err == nil && p > 0 {
			ports.REST = p
		}
	}
	return ports
}

func cmdNodeRestart() *cli.Command {
	return &cli.Command{
		Name:  "restart",
		Usage: "Stop + start the managed lnd using the network / lnddir already in config",
		Flags: []cli.Flag{
			&cli.DurationFlag{Name: "wait-timeout", Value: 30 * time.Second, Usage: "how long to wait for lnd's RPC port to come up after restart"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.LndDir == "" {
				return fail("node restart: no managed lnd recorded in config — run `lokapay node install` + `lokapay node start` first")
			}
			network := sdk.SuiNetwork(cfg.Node.Network)
			netCfg, ok := sdk.SuiNetworkConfigs[network]
			if !ok {
				return fail("node restart: unknown saved network %q", cfg.Node.Network)
			}
			if alreadyRunning(cfg.Node.LndDir) {
				if err := stopManagedLnd(c.Context, cfg); err != nil {
					return fail("node restart: stop failed: %v", err)
				}
				// Give the OS a beat to release the RPC port.
				time.Sleep(1500 * time.Millisecond)
			}
			ports := derivedPorts(cfg)
			if err := spawnManagedLnd(c.Context, cfg, cfg.Node.LndDir, network, netCfg, cfg.Node.PackageID, ports, c.Duration("wait-timeout")); err != nil {
				return fail("node restart: %v", err)
			}
			return nil
		},
	}
}

func cmdNodeUpgrade() *cli.Command {
	return &cli.Command{
		Name:  "upgrade",
		Usage: "Install a newer loka-lnd release, stop the running node, restart on the new binary",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "version", Value: "latest", Usage: "target release tag, e.g. v0.22.0 (or 'latest')"},
			&cli.BoolFlag{Name: "force", Usage: "redownload + overwrite the target version even if already installed"},
			&cli.DurationFlag{Name: "wait-timeout", Value: 30 * time.Second, Usage: "how long to wait for the new lnd's RPC port to come up"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.LndDir == "" {
				return fail("node upgrade: no managed lnd recorded — run `lokapay node install` + `lokapay node start` first")
			}
			destRoot, err := lokapayLndRoot()
			if err != nil {
				return err
			}
			res, err := sdk.DownloadAndExtractLnd(c.Context, destRoot, c.String("version"), c.Bool("force"), os.Stderr)
			if err != nil {
				return fail("node upgrade: %v", err)
			}
			if alreadyRunning(cfg.Node.LndDir) {
				fmt.Fprintln(os.Stderr, "→ stopping running lnd before switching binaries …")
				if err := stopManagedLnd(c.Context, cfg); err != nil {
					return fail("node upgrade: stop failed: %v", err)
				}
				time.Sleep(1500 * time.Millisecond)
			}
			cfg.Node.LndBinaryPath = res.LndPath
			cfg.Node.LncliBinaryPath = res.LncliPath
			cfg.Node.LndVersion = res.Version
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			network := sdk.SuiNetwork(cfg.Node.Network)
			netCfg, ok := sdk.SuiNetworkConfigs[network]
			if !ok {
				return fail("node upgrade: unknown saved network %q", cfg.Node.Network)
			}
			// Re-resolve the package id from the new version's deploy_state.
			fmt.Fprintf(os.Stderr, "→ re-resolving package id for %s @ %s …\n", network, res.Version)
			pkgID, err := sdk.FetchSuiPackageID(c.Context, network, res.Version)
			if err != nil {
				return fail("node upgrade: %v (override with --package-id on the next `node start` if needed)", err)
			}
			cfg.Node.PackageID = pkgID
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			ports := derivedPorts(cfg)
			if err := spawnManagedLnd(c.Context, cfg, cfg.Node.LndDir, network, netCfg, pkgID, ports, c.Duration("wait-timeout")); err != nil {
				return fail("node upgrade: spawn new binary: %v", err)
			}
			fmt.Fprintf(os.Stderr, "✓ upgraded to %s\n", res.Version)
			return nil
		},
	}
}

func cmdNodeStatus() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Show pid / pubkey / balance / peer count of the managed lnd",
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.LndDir == "" {
				fmt.Fprintln(os.Stderr, "no managed lnd recorded — run `lokapay node install` then `lokapay node start`")
				return nil
			}
			pid, _ := readPidFile(cfg.Node.LndDir)
			running := pid > 0 && processAlive(pid)
			fmt.Fprintf(os.Stderr, "managed lnd:\n  network:   %s\n  lnddir:    %s\n  endpoint:  %s\n  pid:       %d %s\n",
				cfg.Node.Network, cfg.Node.LndDir, cfg.Node.Endpoint, pid, ifThen(running, "(alive)", "(NOT running)"))
			if !running {
				return nil
			}
			if out, err := runLncli(c.Context, cfg, "getinfo"); err == nil {
				var info struct {
					IdentityPubkey string `json:"identity_pubkey"`
					Alias          string `json:"alias"`
					BlockHeight    int    `json:"block_height"`
					NumActiveChans int    `json:"num_active_channels"`
					NumPeers       int    `json:"num_peers"`
					Version        string `json:"version"`
				}
				_ = json.Unmarshal([]byte(out), &info)
				fmt.Fprintf(os.Stderr, "  pubkey:    %s\n  alias:     %s\n  version:   %s\n  height:    %d\n  peers:     %d\n  channels:  %d (active)\n",
					info.IdentityPubkey, info.Alias, info.Version, info.BlockHeight, info.NumPeers, info.NumActiveChans)
			}
			if out, err := runLncli(c.Context, cfg, "walletbalance"); err == nil {
				var wb struct {
					ConfirmedBalance string `json:"confirmed_balance"`
				}
				_ = json.Unmarshal([]byte(out), &wb)
				fmt.Fprintf(os.Stderr, "  on-chain:  %s MIST\n", wb.ConfirmedBalance)
			}
			return nil
		},
	}
}

func cmdNodeLogs() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "Tail the managed lnd log",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow output (like tail -F)"},
			&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: 50, Usage: "show last N lines initially"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Node.LndDir == "" {
				return fail("node logs: no managed lnd recorded in config")
			}
			logPath := filepath.Join(cfg.Node.LndDir, "lnd.log")
			args := []string{"-n", strconv.Itoa(c.Int("lines"))}
			if c.Bool("follow") {
				args = append(args, "-F")
			}
			args = append(args, logPath)
			cmd := exec.Command("tail", args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		},
	}
}

// managedLndPorts groups the three ports lnd binds. Kept as a struct
// so the wizard's defaults and the `node start` flag values pass the
// same shape into spawnManagedLnd.
type managedLndPorts struct {
	P2P  int
	RPC  int
	REST int
}

// defaultManagedLndPorts matches loka-agentic-payment SKILL.md and the
// itest_sui_single_coin.sh script.
var defaultManagedLndPorts = managedLndPorts{P2P: 9735, RPC: 10009, REST: 8081}

// localnetManagedLndPorts dodges itest_sui_single_coin.sh, which already
// occupies REST 8081/8082, RPC 10009/10010, and P2P 10011/10012 on the
// loopback. Picking the next free slot keeps lokapay's lnd from colliding
// with alice/bob when both are running on the same dev box.
var localnetManagedLndPorts = managedLndPorts{P2P: 9736, RPC: 10013, REST: 8083}

// managedLndPortsFor returns the port set lokapay's own lnd should use
// for a given Sui network. Only localnet needs the dodged set; the
// public networks use the well-known defaults from SKILL.md.
func managedLndPortsFor(network sdk.SuiNetwork) managedLndPorts {
	if network == sdk.NetworkLocalnet {
		return localnetManagedLndPorts
	}
	return defaultManagedLndPorts
}

// spawnManagedLnd starts lnd in the background and waits for the RPC
// port to come up. Used by both `lokapay node start` and the init
// wizard's guided node path so they share the exact same command-line
// flags / log-handling / PID-file conventions.
//
// Wallet handling: --noseedbackup tells lnd to create the wallet on
// first boot using its built-in default password, with no
// seed-confirmation TTY prompt, and to auto-unlock on every subsequent
// boot. (lnd-sui rejects pairing it with --wallet-unlock-password-file,
// so we don't.) Trust model: anyone with read access to <lndDir> can
// drive the lnd anyway via the macaroon, so this trades zero security
// for full hands-off bootstrap.
//
// The caller is responsible for writing config (endpoint, paths) — this
// helper only manages the process. timeout is how long to wait for RPC.
func spawnManagedLnd(ctx context.Context, cfg *Config, lndDir string, network sdk.SuiNetwork, netCfg sdk.SuiNetworkConfig, pkgID string, ports managedLndPorts, timeout time.Duration) error {
	if err := os.MkdirAll(lndDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", lndDir, err)
	}
	// lnd has no --suinode.localnet flag — localnet is the same wire
	// protocol as devnet, only the RPC host differs. Map via
	// sdk.LndSuiNetworkFlag so the upstream binary stays happy.
	suinodeFlag := sdk.LndSuiNetworkFlag(network)
	// --noseedbackup is sufficient on its own: lnd creates the wallet
	// with its built-in default password on first boot and auto-unlocks
	// it on every subsequent restart. Combining it with
	// --wallet-unlock-password-file is rejected by lnd-sui's
	// ValidateConfig ("cannot set noseedbackup and wallet-unlock-password-file
	// at the same time") — this matches the flag set itest_sui_single_coin.sh
	// uses for alice + bob.
	args := []string{
		"--suinode.active",
		fmt.Sprintf("--suinode.%s", suinodeFlag),
		"--suinode.rpchost=" + netCfg.RPCHost,
		"--suinode.packageid=" + pkgID,
		fmt.Sprintf("--listen=0.0.0.0:%d", ports.P2P),
		fmt.Sprintf("--rpclisten=127.0.0.1:%d", ports.RPC),
		fmt.Sprintf("--restlisten=127.0.0.1:%d", ports.REST),
		"--protocol.wumbo-channels",
		"--protocol.no-anchors",
		"--noseedbackup",
		"--lnddir=" + lndDir,
	}
	logPath := filepath.Join(lndDir, "lnd.log")
	pidPath := filepath.Join(lndDir, "lnd.pid")
	cmd := exec.Command(cfg.Node.LndBinaryPath, args...)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open lnd log %s: %w", logPath, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach so the lnd outlives this lokapay invocation. The platform-
	// specific syscall flags live in process_{unix,windows}.go.
	setDetached(cmd)

	// Pre-flight: refuse to spawn if RPC port is already taken. A stale
	// lnd from a previous run that didn't get cleaned up will otherwise
	// cause the new lnd to die immediately with `bind: address already
	// in use`, while waitForRPC happily probes the old listener and
	// falsely reports success.
	if portInUse(ports.RPC) {
		return fmt.Errorf("RPC port %d is already in use — run `lokapay node stop` (or kill the rogue lnd) before spawning a new one", ports.RPC)
	}

	fmt.Fprintf(os.Stderr, "→ starting %s --suinode.%s …\n", cfg.Node.LndBinaryPath, suinodeFlag)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn lnd: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	// Short post-spawn settle window: catch immediate crashes (bad
	// flags, bind error, segfault) before we go probe the RPC port and
	// possibly mistake an *unrelated* listener for our own lnd.
	time.Sleep(1500 * time.Millisecond)
	if !processAlive(cmd.Process.Pid) {
		_ = os.Remove(pidPath)
		return fmt.Errorf("lnd died immediately after start — see %s", logPath)
	}

	rpcAddr := fmt.Sprintf("127.0.0.1:%d", ports.RPC)
	if err := waitForRPC(ctx, rpcAddr, timeout); err != nil {
		return fmt.Errorf("%w (see %s for details)", err, logPath)
	}
	// Macaroon doesn't exist until wallet creation finishes; lncli won't
	// authenticate without it. Wait for it here so downstream
	// runLncli / connect-peers / openchannel calls don't race a
	// half-initialised wallet.
	macPath := filepath.Join(lndDir, "data", "chain", "sui", string(suinodeFlag), "admin.macaroon")
	if err := waitForFile(ctx, macPath, timeout); err != nil {
		return fmt.Errorf("%w (lnd RPC up but wallet not initialised; check %s)", err, logPath)
	}
	// Macaroon existing means wallet is created, but lnd still has a
	// sub-server warmup window where LightningService RPC calls return
	// "the RPC server is in the process of starting up, but not yet
	// ready to accept calls". The caller (faucet, connect, openchannel)
	// would otherwise hit this race. Poll lncli getinfo until it
	// stops returning that specific error.
	//
	// The caller writes cfg.Node.* (Endpoint / RPCPort / paths) only
	// *after* spawnManagedLnd returns, so we synthesise a scratch
	// config with the right addresses for the readiness probe.
	readyCfg := *cfg
	readyCfg.Node.LndDir = lndDir
	readyCfg.Node.RPCPort = ports.RPC
	readyCfg.Node.TLSCertPath = filepath.Join(lndDir, "tls.cert")
	readyCfg.Node.MacaroonPath = macPath
	if err := waitForLncliReady(ctx, &readyCfg, timeout); err != nil {
		return fmt.Errorf("%w (lnd wallet up but sub-servers not ready; check %s)", err, logPath)
	}
	fmt.Fprintf(os.Stderr, "✓ lnd up (pid=%d rpc=%s log=%s)\n", cmd.Process.Pid, rpcAddr, logPath)
	return nil
}

// waitForLncliReady polls `lncli getinfo` until the sub-server warmup
// is finished. lnd surfaces a recognisable transient error during the
// gap between wallet-unlock and LightningService activation
// ("the RPC server is in the process of starting up") — any other
// error is treated as fatal so we don't loop on a real misconfiguration.
//
// Requires cfg.Node.RPCPort / TLSCertPath / MacaroonPath / LndDir to be
// set; spawnManagedLnd is its only caller and writes them before
// spawning.
func waitForLncliReady(ctx context.Context, cfg *Config, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := runLncli(ctx, cfg, "getinfo")
		if err == nil {
			return nil
		}
		lastErr = err
		msg := err.Error()
		if !strings.Contains(msg, "is in the process of starting up") &&
			!strings.Contains(msg, "wallet locked") &&
			!strings.Contains(msg, "Unavailable") {
			// A non-transient error — surface it now instead of timing out.
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("lnd sub-servers did not become ready within %s", timeout)
	}
	return lastErr
}

// portInUse returns true if something is already listening on the given
// loopback TCP port. Used as a pre-flight before spawning lnd so we
// fail with a clear message instead of letting the new lnd die silently.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForFile polls until path exists or timeout elapses. Used to wait
// for lnd's admin.macaroon, which only appears after wallet creation
// completes — RPC liveness alone isn't enough.
func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("file %s did not appear within %s", path, timeout)
}

// ----------------------------------------------------------------
// Helpers shared by the subcommands.

func lokapayLndRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".lokapay", "lnd"), nil
}

func lokapayLndDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".lokapay", "lnd-data"), nil
}

func waitForRPC(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("lnd RPC at %s didn't come up within %s", addr, timeout)
}

func alreadyRunning(lndDir string) bool {
	pid, err := readPidFile(lndDir)
	if err != nil {
		return false
	}
	return processAlive(pid)
}

func readPidFile(lndDir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(lndDir, "lnd.pid"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func removePidFile(lndDir string) {
	_ = os.Remove(filepath.Join(lndDir, "lnd.pid"))
}

// processAlive is provided by process_unix.go / process_windows.go via
// signalProcessAlive — the cross-platform wrapper. Kept here as a
// pid-only entry point so existing callers don't change.
func processAlive(pid int) bool {
	return signalProcessAlive(pid)
}

// runLncli shells out to lncli with the right --rpcserver / --lnddir /
// --macaroonpath / --no-macaroons options derived from cfg. Returns
// stdout on success; stderr is bubbled up in the error.
func runLncli(ctx context.Context, cfg *Config, args ...string) (string, error) {
	if cfg.Node.LncliBinaryPath == "" {
		return "", errors.New("no lncli binary recorded in config — run `lokapay node install`")
	}
	full := []string{
		"--lnddir=" + cfg.Node.LndDir,
	}
	// Always pass --rpcserver explicitly when we know the port. lncli's
	// default is 127.0.0.1:10009, which on a localnet box also happens
	// to be itest_sui_single_coin.sh's alice — sending lokapay's
	// commands there with lokapay's tls.cert fails CA verification.
	if cfg.Node.RPCPort > 0 {
		full = append(full, fmt.Sprintf("--rpcserver=127.0.0.1:%d", cfg.Node.RPCPort))
	}
	if cfg.Node.TLSCertPath != "" {
		full = append(full, "--tlscertpath="+cfg.Node.TLSCertPath)
	}
	if cfg.Node.MacaroonPath != "" {
		full = append(full, "--macaroonpath="+cfg.Node.MacaroonPath)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, cfg.Node.LncliBinaryPath, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("lncli %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// fundWalletFromFaucet derives a Sui address via lncli newaddress and
// requests the network's faucet to drop test SUI on it. Skips silently
// on mainnet (no faucet).
func fundWalletFromFaucet(ctx context.Context, cfg *Config, netCfg sdk.SuiNetworkConfig) error {
	if netCfg.FaucetURL == "" {
		return nil
	}
	out, err := runLncli(ctx, cfg, "newaddress", "p2wkh")
	if err != nil {
		return fmt.Errorf("derive address: %w", err)
	}
	var addr struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal([]byte(out), &addr); err != nil {
		return fmt.Errorf("parse newaddress: %w", err)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"FixedAmountRequest": map[string]string{"recipient": addr.Address},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, netCfg.FaucetURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", netCfg.FaucetURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("faucet returned status %d for %s", resp.StatusCode, addr.Address)
	}
	fmt.Fprintf(os.Stderr, "✓ faucet request accepted for %s — wait ~30s for funding to settle\n", addr.Address)
	return nil
}

// connectPeers wires lokapay's lnd up to the right starter set of peers
// for its network: public Loka seed nodes for devnet/testnet/mainnet, or
// the itest_sui_single_coin.sh alice + bob nodes for localnet (whose
// identity pubkeys are regenerated on every run, so they're resolved
// at connect-time via the peer's lnd REST /v1/getinfo).
//
// Returns the list of successfully-connected peer addresses (pubkey@host:port).
// Failures are warned about, not fatal — node bootstrap should still
// succeed even if one peer is unreachable.
func connectPeers(ctx context.Context, cfg *Config, network sdk.SuiNetwork) []string {
	if network == sdk.NetworkLocalnet {
		return connectLocalnetItestPeers(ctx, cfg, network)
	}
	return connectPublicSeeds(ctx, cfg)
}

func connectPublicSeeds(ctx context.Context, cfg *Config) []string {
	connected := make([]string, 0, len(sdk.LokaSeedNodes))
	for _, seed := range sdk.LokaSeedNodes {
		// "already connected" is normal and harmless (SKILL.md gotcha #7).
		if _, err := runLncli(ctx, cfg, "connect", seed); err != nil {
			if strings.Contains(err.Error(), "already connected") {
				connected = append(connected, seed)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: connect %s failed: %v\n", seed, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "✓ connected to %s\n", seed)
		connected = append(connected, seed)
	}
	return connected
}

func connectLocalnetItestPeers(ctx context.Context, cfg *Config, network sdk.SuiNetwork) []string {
	connected := make([]string, 0, len(sdk.LocalnetItestPeers))
	for _, peer := range sdk.LocalnetItestPeers {
		macPath := peer.MacaroonPath(network)
		pubkey, err := sdk.FetchLndIdentityPubkey(ctx, peer.RESTAddr, macPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: resolve %s pubkey via %s failed: %v\n",
				peer.Name, peer.RESTAddr, err)
			fmt.Fprintf(os.Stderr, "         (is itest_sui_single_coin.sh running? expected macaroon at %s)\n", macPath)
			continue
		}
		addr := fmt.Sprintf("%s@%s", pubkey, peer.P2PAddr)
		if _, err := runLncli(ctx, cfg, "connect", addr); err != nil {
			if strings.Contains(err.Error(), "already connected") {
				connected = append(connected, addr)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: connect %s (%s) failed: %v\n", peer.Name, addr, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "✓ connected to %s (%s)\n", peer.Name, addr)
		connected = append(connected, addr)
	}
	return connected
}

// openLocalnetChannel funds + broadcasts a channel-open from lokapay's
// lnd to one of the itest peers (alice by default). Best-effort: any
// error is reported but does not break the init flow — the user can
// open the channel manually with `lokapay node openchannel` later.
//
// localAmt is in MIST sats (1 sat == 1 MIST on Sui-backed lnd). 5 SUI
// (5,000,000,000) is the default — enough headroom for hundreds of
// small L402 payments without exhausting the lokapay wallet's faucet
// balance.
func openLocalnetChannel(ctx context.Context, cfg *Config, network sdk.SuiNetwork, peerName string, localAmt int64) error {
	var target sdk.LocalnetItestPeer
	found := false
	for _, p := range sdk.LocalnetItestPeers {
		if p.Name == peerName {
			target = p
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown itest peer %q", peerName)
	}
	macPath := target.MacaroonPath(network)
	pubkey, err := sdk.FetchLndIdentityPubkey(ctx, target.RESTAddr, macPath)
	if err != nil {
		return fmt.Errorf("resolve %s pubkey: %w", peerName, err)
	}
	out, err := runLncli(ctx, cfg, "openchannel",
		"--node_key="+pubkey,
		fmt.Sprintf("--local_amt=%d", localAmt))
	if err != nil {
		return fmt.Errorf("openchannel: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ openchannel → %s (%s, %d sat / MIST)\n", peerName, pubkey, localAmt)
	if trimmed := strings.TrimSpace(out); trimmed != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", trimmed)
	}
	return nil
}

func ifThen[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
