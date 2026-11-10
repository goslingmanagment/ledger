package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "keys create", Run: keysCreate,
		Summary: "выпустить ключ агента (на сервере): ledger keys create <имя> --scope read[,propose,decide,collect,import,reports] | payouts [--replace]"})
	Command(Cmd{Name: "keys list", Summary: "ключи агентов: права, когда выпущены, использованы, отозваны [--json]", Run: keysList})
	Command(Cmd{Name: "keys revoke", Summary: "отозвать ключ агента: ledger keys revoke <имя>", Run: keysRevoke})
}

func keysCreate(ctx context.Context, args []string) error {
	fs := Flags("keys create")
	scope := fs.String("scope", auth.ScopeRead, "права через запятую: "+strings.Join(auth.Scopes, ", "))
	replace := fs.Bool("replace", false, "имя занято: выпустить новый ключ, прежний отозвать")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужно одно имя: ledger keys create <имя> --scope read")
	}
	scopes, err := auth.ParseScopes(*scope)
	if err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	token, k, err := auth.Create(ctx, store.RW, pos[0], scopes, *replace, ts.Now())
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Ключ показан один раз. Агенту: ~/.config/ledger/credentials (chmod 600), строки LEDGER_URL=… и LEDGER_KEY=<ключ>.")
	return printJSON(map[string]any{"ok": true, "name": k.Name, "scopes": k.Scopes, "prefix": k.Prefix, "key": token})
}

func keysList(ctx context.Context, args []string) error {
	fs := Flags("keys list")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	keys, err := auth.List(ctx, store.RO)
	if err != nil {
		return err
	}
	if *asJSON {
		if keys == nil {
			keys = []auth.Key{}
		}
		return printJSON(map[string]any{"ok": true, "keys": keys})
	}
	dash := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	rows := make([][]string, len(keys))
	for i, k := range keys {
		rows[i] = []string{k.Name, strings.Join(k.Scopes, ","), k.Prefix + "…", k.CreatedAt, dash(k.LastUsedAt), dash(k.RevokedAt)}
	}
	out := bufio.NewWriter(os.Stdout)
	writeTable(out, []string{"name", "scopes", "prefix", "created_at", "last_used_at", "revoked_at"}, rows, nil)
	return out.Flush()
}

func keysRevoke(ctx context.Context, args []string) error {
	fs := Flags("keys revoke")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("нужно одно имя: ledger keys revoke <имя>")
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	k, err := auth.Revoke(ctx, store.RW, fs.Arg(0), ts.Now())
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "name": k.Name, "revokedAt": k.RevokedAt})
}
