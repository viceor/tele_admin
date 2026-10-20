package store

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Operator tooling for template records (MYC-83). Both entry points work on
// a database file directly, with no Store attached. The service must be
// stopped first: opening the FileStore takes the single-writer lock.
//
// Startup refuses ownerless template records (owner 0; negative owners are
// valid group/channel chat ids, so only 0 counts as ownerless, fail-closed).
// These tools make that state actionable without destroying data:
//
//   - DiagnoseTemplates reports every template record by category. It never
//     modifies the database.
//   - QuarantineOwnerlessTemplates moves ownerless records into the
//     templates_quarantine table, with their content intact. Startup then
//     proceeds. Nothing is deleted, and an operator can inspect or restore
//     the records later.
//
// Legacy bare-name records that do have an owner need no action: startup
// upgrades them to the namespaced key automatically.

// templateQuarantineTable holds ownerless template records moved aside by
// QuarantineOwnerlessTemplates. The store never reads this table.
const templateQuarantineTable = "templates_quarantine"

// TemplateReport classifies the template records of one database file. Each
// slice holds record keys, sorted. Keys are chat-scoped identifiers, not
// template contents.
type TemplateReport struct {
	// Namespaced records ("<owner>/<name>") are loadable as they are.
	Namespaced []string
	// Legacy records use a bare name and have an owner. They are upgraded
	// to the namespaced key on the next startup.
	Legacy []string
	// Ownerless records have no owning chat. Startup refuses them.
	Ownerless []string
	// Malformed records cannot be decoded. Startup refuses them too.
	Malformed []string
	// Quarantined records were moved aside by QuarantineOwnerlessTemplates.
	Quarantined []string
}

// DiagnoseTemplates classifies every record of the "templates" table and lists
// the quarantine table. It only reads.
func DiagnoseTemplates(fs *FileStore) TemplateReport {
	var rep TemplateReport
	for _, key := range fs.Keys("templates") {
		var rec persistedTemplateRecord
		ok, err := fs.Get("templates", key, &rec)
		switch {
		case err != nil || !ok:
			rep.Malformed = append(rep.Malformed, key)
		case rec.Owner == 0:
			rep.Ownerless = append(rep.Ownerless, key)
		case strings.Contains(key, "/"):
			rep.Namespaced = append(rep.Namespaced, key)
		default:
			rep.Legacy = append(rep.Legacy, key)
		}
	}
	rep.Quarantined = fs.Keys(templateQuarantineTable)
	return rep
}

// QuarantineOwnerlessTemplates moves every ownerless template record into the
// quarantine table in one transaction. Each record keeps its key and its
// exact stored JSON. It refuses to run if a quarantined key already exists,
// so a previous quarantine is never overwritten. It returns the moved keys.
// When nothing is ownerless it changes nothing.
func QuarantineOwnerlessTemplates(fs *FileStore) ([]string, error) {
	rep := DiagnoseTemplates(fs)
	if len(rep.Ownerless) == 0 {
		return nil, nil
	}
	already := make(map[string]bool, len(rep.Quarantined))
	for _, k := range rep.Quarantined {
		already[k] = true
	}
	for _, k := range rep.Ownerless {
		if already[k] {
			return nil, fmt.Errorf("template %q is already in %q; resolve the existing quarantine record before moving another one over it", k, templateQuarantineTable)
		}
	}
	tx, err := fs.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin quarantine: %w", err)
	}
	for _, k := range rep.Ownerless {
		var raw json.RawMessage
		if ok, err := fs.Get("templates", k, &raw); err != nil || !ok {
			tx.Rollback()
			return nil, fmt.Errorf("read ownerless template %q for quarantine: found=%v err=%v", k, ok, err)
		}
		if err := tx.Put(templateQuarantineTable, k, raw); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("stage quarantine of %q: %w", k, err)
		}
		if err := tx.Delete("templates", k); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("stage removal of %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit quarantine: %w", err)
	}
	moved := append([]string(nil), rep.Ownerless...)
	sort.Strings(moved)
	return moved, nil
}

// RunTemplateMaintenance opens the database at path, prints the template
// report to out, and, when quarantine is true, moves ownerless records aside.
// It returns an error when the database cannot be opened, or when quarantine
// fails.
func RunTemplateMaintenance(path string, quarantine bool, out io.Writer) error {
	fs, err := OpenFileStore(path)
	if err != nil {
		return fmt.Errorf("open database %q: %w", path, err)
	}
	defer fs.Close()
	rep := DiagnoseTemplates(fs)
	printReport(out, path, rep)
	if !quarantine {
		if len(rep.Ownerless) > 0 {
			fmt.Fprintf(out, "\n%d ownerless template record(s) block startup. Re-run with -db-quarantine-ownerless-templates to move them to %q (content is kept).\n", len(rep.Ownerless), templateQuarantineTable)
		}
		return nil
	}
	moved, err := QuarantineOwnerlessTemplates(fs)
	if err != nil {
		return err
	}
	if len(moved) == 0 {
		fmt.Fprintln(out, "\nno ownerless template records; nothing moved.")
		return nil
	}
	fmt.Fprintf(out, "\nmoved %d ownerless template record(s) to %q: %s\n", len(moved), templateQuarantineTable, strings.Join(moved, ", "))
	return nil
}

func printReport(out io.Writer, path string, rep TemplateReport) {
	fmt.Fprintf(out, "database: %s\n", path)
	fmt.Fprintf(out, "templates namespaced (loadable): %d\n", len(rep.Namespaced))
	fmt.Fprintf(out, "templates legacy bare-name with owner (upgraded at startup): %d %v\n", len(rep.Legacy), rep.Legacy)
	fmt.Fprintf(out, "templates ownerless (startup refused): %d %v\n", len(rep.Ownerless), rep.Ownerless)
	fmt.Fprintf(out, "templates malformed (startup refused): %d %v\n", len(rep.Malformed), rep.Malformed)
	fmt.Fprintf(out, "quarantined: %d %v\n", len(rep.Quarantined), rep.Quarantined)
}
