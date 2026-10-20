package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/media"
)

func init() {
	register("media", "list, withhold or restore the NFT media this node stores", runMedia)
}

// runMedia is the operator's side of the media store. It opens the node's
// database directly, so it is run inside the node's container:
//
//	docker exec zvm-node zvm media list -db /data/zvm.db
func runMedia(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zvm media <list|hide|show|info> [flags] [hash]")
	}
	fs := flag.NewFlagSet("media "+args[0], flag.ExitOnError)
	dbPath := fs.String("db", "/data/zvm.db", "the node's database")
	limit := fs.Int("limit", 50, "how many objects to list")
	fs.Parse(args[1:])

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	hash := fs.Arg(0)
	switch args[0] {
	case "list":
		objects, err := media.List(db.SQL(), *limit)
		if err != nil {
			return err
		}
		for _, o := range objects {
			state := "served"
			if o.Hidden {
				state = "withheld"
			}
			fmt.Printf("%s  %-16s %9d B  %-8s %s  by %s\n",
				o.Hash, o.ContentType, o.Size, state, time.Unix(o.CreatedAt, 0).UTC().Format(time.RFC3339), o.Uploader)
		}
		fmt.Printf("%d object(s)\n", len(objects))
		return nil
	case "info":
		o, err := media.Describe(db.SQL(), hash)
		if err != nil {
			return err
		}
		fmt.Printf("hash        %s\ntype        %s\nsize        %d bytes\nuploaded by %s at %s\nwithheld    %v\n",
			o.Hash, o.ContentType, o.Size, o.Uploader, time.Unix(o.CreatedAt, 0).UTC().Format(time.RFC3339), o.Hidden)
		return nil
	case "hide", "show":
		if hash == "" {
			return fmt.Errorf("usage: zvm media %s [flags] <hash>", args[0])
		}
		hide := args[0] == "hide"
		if err := media.SetHidden(db.SQL(), hash, hide); err != nil {
			return err
		}
		if hide {
			fmt.Printf("%s is withheld: this node answers 451 for it.\n", hash)
			fmt.Println("A CDN in front of this node still holds its own copy — purge that URL there too (docs/CDN.md).")
		} else {
			fmt.Printf("%s is served again.\n", hash)
		}
		return nil
	default:
		return fmt.Errorf("unknown media command %q", args[0])
	}
}
