package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/goslingmanagment/ledger/internal/free"
)

func init() {
	Command(Cmd{Name: "free", Summary: "сколько можно вывести сейчас и после ближайших переводов [--json]", Run: freeCmd})
	Command(Cmd{Name: "import commitments", Summary: "обязательства из JSON: ledger import commitments <файл>", Run: importCommitments})
}

func freeCmd(ctx context.Context, args []string) error {
	fs := Flags("free")
	asJSON := fs.Bool("json", false, "весь ответ JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if local() {
		store, err := openDirect(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		a, err := free.Compute(ctx, store.RO, time.Now())
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(a)
		}
		fmt.Println(a.Render())
		return nil
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	var out struct {
		Text string          `json:"text"`
		Free json.RawMessage `json:"free"`
	}
	if err := c.Do(ctx, "GET", "/v1/free", nil, &out); err != nil {
		return apiError(err)
	}
	if *asJSON {
		fmt.Println(string(out.Free))
		return nil
	}
	fmt.Println(out.Text)
	return nil
}

func importCommitments(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен JSON-файл")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var items []free.CommitmentSeed
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n, err := free.ImportCommitments(ctx, tx, items, "commitments.json", time.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "written": n})
}
