package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kidandcat/takan/internal/config"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules/bots"
)

// runtimeBundleUsage is printed for `takan bundle` with no or bad arguments.
const runtimeBundleUsage = `takan bundle import — capture the runtime bundle a provisioned bot needs.

Run it on the hub host, as the user that owns the Takan data directory:

  takan bundle import \
    --grok-home /home/debian/.grok \
    --atlas-data /home/debian/atlas-data \
    --env /home/debian/atlas.env

It reads those files and nothing else: no file is written, executed, chowned or
chmodded on the source side, and the grok CLI is never invoked (invoking it
refreshes and re-owns auth.json, which is how a live daemon gets broken).
Afterwards it asserts that every source file still has the ownership and mode it
had before. The result is sealed with TAKAN_SESSION_KEY and stored in the hub
database; nothing readable is written anywhere.`

// runBundleCLI handles `takan bundle …`. It returns the process exit code.
func runBundleCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, runtimeBundleUsage)
		return 2
	}
	switch args[0] {
	case "import":
		return runBundleImport(args[1:])
	case "status":
		return runBundleStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown bundle subcommand %q\n\n%s\n", args[0], runtimeBundleUsage)
		return 2
	}
}

func runBundleImport(args []string) int {
	fs := flag.NewFlagSet("bundle import", flag.ContinueOnError)
	grokHome := fs.String("grok-home", "", "grok home to read auth.json and config.toml from")
	agentData := fs.String("atlas-data", "", "daemon data dir holding config.toml and workspace/AGENTS.md")
	envFile := fs.String("env", "", "daemon environment file to read GROQ_API_KEY from")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *grokHome == "" || *agentData == "" {
		fmt.Fprintln(os.Stderr, runtimeBundleUsage)
		return 2
	}

	paths := bots.ImportPaths{GrokHome: *grokHome, AgentData: *agentData, EnvFile: *envFile}
	// Snapshot ownership of everything ReadBundle will touch, so a stat that
	// itself fails is reported before anything is stored.
	before, err := bots.StatOwners(guardedPaths(paths))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: %v\n", err)
		return 1
	}
	res, err := bots.ReadBundle(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: %v\n", err)
		return 1
	}
	if err := bots.AssertOwnersUnchanged(before); err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: %v\n", err)
		return 1
	}

	st, box, err := openHub()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: %v\n", err)
		return 1
	}
	defer st.Close()

	ctx := context.Background()
	owner, err := st.Owner(ctx)
	if err != nil || owner == nil {
		fmt.Fprintln(os.Stderr, "bundle import: this instance has no owner account yet")
		return 1
	}
	sealed, err := bots.SealBundle(box, res.Bundle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: seal: %v\n", err)
		return 1
	}
	row := &store.RuntimeBundle{
		UserID:        owner.ID,
		GrokVersion:   res.Bundle.GrokVersion,
		SourceName:    res.Name,
		SourceSlug:    res.Slug,
		SourceDataDir: res.DataDir,
		PayloadEnc:    sealed,
		Components:    res.Bundle.Components(),
	}
	if err := st.SaveRuntimeBundle(ctx, row); err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: save: %v\n", err)
		return 1
	}
	// Re-assert after the write: a bug that shelled out or copied with the
	// wrong flags would show up here, before anyone trusts the bundle.
	if err := bots.AssertOwnersUnchanged(before); err != nil {
		fmt.Fprintf(os.Stderr, "bundle import: %v\n", err)
		return 1
	}

	fmt.Printf("runtime bundle stored for %s (source %s, grok %s)\n",
		owner.Email, res.Name, orDash(res.Bundle.GrokVersion))
	for _, c := range row.Components {
		fmt.Printf("  · %-22s %d bytes\n", c.Name, c.Bytes)
	}
	fmt.Println("source files unchanged (owner, group and mode verified)")
	return 0
}

func runBundleStatus(args []string) int {
	fs := flag.NewFlagSet("bundle status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, _, err := openHub()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle status: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	owner, err := st.Owner(ctx)
	if err != nil || owner == nil {
		fmt.Fprintln(os.Stderr, "bundle status: this instance has no owner account yet")
		return 1
	}
	row, err := st.RuntimeBundle(ctx, owner.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle status: %v\n", err)
		return 1
	}
	if row == nil {
		fmt.Println("no runtime bundle imported")
		return 0
	}
	fmt.Printf("runtime bundle from %s (grok %s), updated %s\n",
		orDash(row.SourceName), orDash(row.GrokVersion), row.UpdatedAt.Format("2006-01-02 15:04 MST"))
	for _, c := range row.Components {
		fmt.Printf("  · %-22s %d bytes\n", c.Name, c.Bytes)
	}
	return 0
}

// guardedPaths lists every source file the import reads, so ownership can be
// snapshotted before the first read.
func guardedPaths(p bots.ImportPaths) []string {
	var out []string
	add := func(path string) {
		if path == "" {
			return
		}
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			out = append(out, path)
		}
	}
	home := strings.TrimRight(p.GrokHome, "/")
	data := strings.TrimRight(p.AgentData, "/")
	add(home + "/auth.json")
	add(home + "/config.toml")
	add(data + "/config.toml")
	add(data + "/workspace/AGENTS.md")
	add(p.EnvFile)
	return out
}

// openHub opens the store and the sealing box exactly like the server does, so
// the CLI writes rows the running hub can read back.
func openHub() (*store.Store, *cryptox.Box, error) {
	cfg := config.Load()
	if cfg.SessionKey == "dev-insecure-change-me" {
		return nil, nil, fmt.Errorf("TAKAN_SESSION_KEY is unset — run this with the hub's environment file loaded")
	}
	st, err := store.Open(cfg.DataDir, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("store: %w", err)
	}
	box, err := cryptox.NewBox(cfg.SessionKey)
	if err != nil {
		_ = st.Close() // safe-ignore: already failing; the close error would mask the real one
		return nil, nil, fmt.Errorf("crypto: %w", err)
	}
	st.SetOwnerHint(cfg.OwnerEmail)
	return st, box, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
