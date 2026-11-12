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
	Command(Cmd{Name: "keys hub-agent", Run: keysHubAgent,
		Summary: "открыть ledger агенту хаба по его ключу хаба (на сервере): ledger keys hub-agent <код> --scope read[,payouts] [--note …] | --off"})
	Command(Cmd{Name: "keys hub-agents", Summary: "агенты хаба, которым ledger открыт по ключу хаба: права, выключены ли [--json]", Run: keysHubAgents})
}

func keysHubAgent(ctx context.Context, args []string) error {
	fs := Flags("keys hub-agent")
	scope := fs.String("scope", "", "права через запятую, только чтение: "+strings.Join(auth.HubScopes, ", "))
	note := fs.String("note", "", "зачем агенту доступ")
	off := fs.Bool("off", false, "закрыть доступ (строка остаётся)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен один код агента хаба: ledger keys hub-agent <код> --scope read")
	}
	if *off == (*scope != "") {
		return fmt.Errorf("нужно одно из двух: --scope read[,payouts] или --off")
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	var a auth.HubAgent
	if *off {
		a, err = auth.DisableHubAgent(ctx, store.RW, pos[0], ts.Now())
	} else {
		scopes, perr := auth.ParseHubScopes(*scope)
		if perr != nil {
			return perr
		}
		a, err = auth.SetHubAgent(ctx, store.RW, pos[0], scopes, *note, ts.Now())
	}
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "agent": a})
}

func keysHubAgents(ctx context.Context, args []string) error {
	fs := Flags("keys hub-agents")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	agents, err := auth.ListHubAgents(ctx, store.RO)
	if err != nil {
		return err
	}
	if *asJSON {
		if agents == nil {
			agents = []auth.HubAgent{}
		}
		return printJSON(map[string]any{"ok": true, "agents": agents})
	}
	dash := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	rows := make([][]string, len(agents))
	for i, a := range agents {
		rows[i] = []string{a.Code, strings.Join(a.Scopes, ","), a.UpdatedAt, dash(a.DisabledAt), dash(a.Note)}
	}
	out := bufio.NewWriter(os.Stdout)
	writeTable(out, []string{"code", "scopes", "updated_at", "disabled_at", "note"}, rows, nil)
	return out.Flush()
}
