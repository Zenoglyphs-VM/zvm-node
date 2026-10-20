package bench

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// What a node's SQLite store holds, table by table and index by index. It runs
// inside the node's container (`zvm bench storage`) because the database lives
// in a Docker volume the host cannot open, and it opens the file read-only so
// it can run beside the node that is writing it.

type DBObject struct {
	Name    string `json:"name"`
	Table   string `json:"table"`
	Kind    string `json:"kind"` // table or index
	Pages   int64  `json:"pages"`
	Bytes   int64  `json:"bytes"`
	Payload int64  `json:"payload"`
}

type DBTable struct {
	Name       string `json:"name"`
	Rows       int64  `json:"rows"`
	Bytes      int64  `json:"bytes"`      // the table and its indexes
	IndexBytes int64  `json:"indexBytes"` // the indexes alone
}

type DirReport struct {
	Path  string `json:"path"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

type StorageReport struct {
	Path      string           `json:"path"`
	At        int64            `json:"at"`
	TookMS    int64            `json:"tookMs"`
	FileBytes int64            `json:"fileBytes"`
	WALBytes  int64            `json:"walBytes"`
	PageSize  int64            `json:"pageSize"`
	Pages     int64            `json:"pages"`
	FreePages int64            `json:"freePages"`
	Objects   []DBObject       `json:"objects"`
	Tables    []DBTable        `json:"tables"`
	Heads     map[string]int64 `json:"heads"`
	Media     *DirReport       `json:"media,omitempty"`
}

// InspectStorage sizes a node database and, if given, its media directory.
func InspectStorage(ctx context.Context, dbPath, mediaDir string) (*StorageReport, error) {
	start := time.Now()
	rep := &StorageReport{Path: dbPath, At: start.Unix(), Heads: map[string]int64{}}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	rep.FileBytes = info.Size()
	if wal, err := os.Stat(dbPath + "-wal"); err == nil {
		rep.WALBytes = wal.Size()
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro&_pragma=busy_timeout(15000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	for pragma, dst := range map[string]*int64{"page_size": &rep.PageSize, "page_count": &rep.Pages, "freelist_count": &rep.FreePages} {
		if err := db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(dst); err != nil {
			return nil, err
		}
	}

	kinds := map[string][2]string{} // name -> table, kind
	rows, err := db.QueryContext(ctx, "SELECT name, tbl_name, type FROM sqlite_master WHERE type IN ('table', 'index')")
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name, tbl, kind string
		if err := rows.Scan(&name, &tbl, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		kinds[name] = [2]string{tbl, kind}
		if kind == "table" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	rows.Close()

	byTable := map[string]*DBTable{}
	for _, t := range tables {
		byTable[t] = &DBTable{Name: t}
	}
	rows, err = db.QueryContext(ctx, "SELECT name, COUNT(*), SUM(pgsize), SUM(payload) FROM dbstat GROUP BY name")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o DBObject
		if err := rows.Scan(&o.Name, &o.Pages, &o.Bytes, &o.Payload); err != nil {
			rows.Close()
			return nil, err
		}
		if k, ok := kinds[o.Name]; ok {
			o.Table, o.Kind = k[0], k[1]
		} else {
			o.Table, o.Kind = o.Name, "internal"
		}
		rep.Objects = append(rep.Objects, o)
		if t := byTable[o.Table]; t != nil {
			t.Bytes += o.Bytes
			if o.Kind == "index" {
				t.IndexBytes += o.Bytes
			}
		}
	}
	rows.Close()
	sort.Slice(rep.Objects, func(i, j int) bool { return rep.Objects[i].Bytes > rep.Objects[j].Bytes })

	for _, name := range tables {
		t := byTable[name]
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&t.Rows); err != nil {
			return nil, err
		}
		rep.Tables = append(rep.Tables, *t)
	}
	sort.Slice(rep.Tables, func(i, j int) bool { return rep.Tables[i].Bytes > rep.Tables[j].Bytes })

	for key, q := range map[string]string{
		"indexedBlock":    "SELECT COALESCE(MAX(number), 0) FROM blocks",
		"derivedMomentum": "SELECT COALESCE(MAX(height), 0) FROM momentums",
		"derivedBlock":    "SELECT COALESCE(MAX(l2_number), 0) FROM momentums",
	} {
		var v int64
		if db.QueryRowContext(ctx, q).Scan(&v) == nil {
			rep.Heads[key] = v
		}
	}

	if mediaDir != "" {
		d := &DirReport{Path: mediaDir}
		err := filepath.WalkDir(mediaDir, func(_ string, e fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !e.IsDir() {
				if fi, err := e.Info(); err == nil {
					d.Files++
					d.Bytes += fi.Size()
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		rep.Media = d
	}
	rep.TookMS = time.Since(start).Milliseconds()
	return rep, nil
}
