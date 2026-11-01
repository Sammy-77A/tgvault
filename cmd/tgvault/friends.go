package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/friends"
	"github.com/Sammy-77A/tgvault/internal/store"
)

const friendsUsage = `Usage: tgvault friends [--data DIR] <command>

  add NAME [--quota 50GB]   create a friend and print their API key (shown once)
  list                      show everyone, how much they use and their limit
  quota NAME SIZE           change a friend's storage limit (0 = no limit)
  key NAME                  give a friend a new API key (the old one stops working)
  disable NAME              block a friend; their files stay
  enable NAME               unblock a friend
  remove NAME               delete a friend and their share links; their files stay in Telegram

Run it on the same machine and with the same --data folder as 'tgvault --server'.
`

// friendsCommand manages who may use the file server. It edits the same
// database as a running server, which picks changes up immediately.
func friendsCommand(args []string) {
	fs := flag.NewFlagSet("friends", flag.ExitOnError)
	dataFlag := fs.String("data", "", "folder for tgvault's settings and login")
	quota := fs.String("quota", "0", "storage limit, e.g. 50GB (0 = no limit)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, friendsUsage) }
	// Flags may come before or after the command and name.
	var pos []string
	for len(args) > 0 {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	if len(pos) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	st, err := store.Open(filepath.Join(dataPath(*dataFlag), "tgvault.db"))
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	cmd, rest := pos[0], pos[1:]
	name := ""
	if cmd != "list" {
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "tgvault: friends "+cmd+" needs a name")
			os.Exit(2)
		}
		name = strings.ToLower(rest[0])
	}
	check := func(err error) {
		switch {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			fmt.Fprintf(os.Stderr, "tgvault: no friend named %s\n", name)
			os.Exit(1)
		default:
			fmt.Fprintln(os.Stderr, "tgvault:", err)
			os.Exit(1)
		}
	}

	switch cmd {
	case "add":
		if !friends.ValidName(name) {
			fmt.Fprintln(os.Stderr, "tgvault: a name is 2 to 32 lowercase letters, digits, - or _")
			os.Exit(2)
		}
		q := *quota
		if len(rest) > 1 {
			q = rest[1]
		}
		limit, err := friends.ParseSize(q)
		check(err)
		key, hash := friends.NewKey()
		check(st.AddFriend(name, hash, limit))
		fmt.Printf("Added %s (limit: %s).\nAPI key (shown only now, send it to them privately):\n\n  %s\n\n", name, limitText(limit), key)
	case "list":
		list, err := st.Friends()
		check(err)
		if len(list) == 0 {
			fmt.Println("No friends yet. Add one with: tgvault friends add NAME --quota 50GB")
			return
		}
		fmt.Printf("%-20s %-12s %-10s %s\n", "NAME", "LIMIT", "STATUS", "ADDED")
		for _, f := range list {
			status := "active"
			if f.Disabled {
				status = "disabled"
			}
			fmt.Printf("%-20s %-12s %-10s %s\n", f.Name, limitText(f.Quota), status, time.Unix(f.Created, 0).Format("2006-01-02"))
		}
	case "quota":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "tgvault: friends quota NAME SIZE")
			os.Exit(2)
		}
		limit, err := friends.ParseSize(rest[1])
		check(err)
		check(st.SetFriendQuota(name, limit))
		fmt.Printf("%s's limit is now %s.\n", name, limitText(limit))
	case "key":
		key, hash := friends.NewKey()
		check(st.SetFriendKey(name, hash))
		fmt.Printf("New API key for %s (the old one no longer works):\n\n  %s\n\n", name, key)
	case "disable", "enable":
		check(st.SetFriendDisabled(name, cmd == "disable"))
		fmt.Printf("%s is now %sd.\n", name, cmd)
	case "remove":
		check(st.RemoveFriend(name))
		fmt.Printf("Removed %s. Their files are still in your Telegram under %s/%s.\n", name, friends.Folder, name)
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func limitText(n int64) string {
	if n == 0 {
		return "no limit"
	}
	return friends.FormatSize(n)
}
