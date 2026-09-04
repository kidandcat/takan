package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kidandcat/takan/internal/assistant"
)

// schedUsage documents the atlas-sched helper. The CLI agent reads this in
// AGENTS.md and uses it to turn "remind me tomorrow at 9" into a real job.
const schedUsage = `atlas-sched — manage reminders and routines.

Usage:
  atlas-sched list
  atlas-sched add --type message --at "2026-09-05 09:00" --text "call the dentist"
  atlas-sched add --type message --cron "0 9 * * 1" --text "weekly review"
  atlas-sched add --type agent --cron "30 7 * * *" --name morning --text "summarise today's calendar"
  atlas-sched rm <id>
  atlas-sched run <id>

Flags for add:
  --type   message (send --text verbatim) or agent (run the CLI agent on --text and send its output)
  --text   the message, or the prompt for an agent job (required)
  --at     one-shot time in Europe/Madrid: "2026-09-05 09:00", RFC3339, or relative "+90m" / "+2h"
  --cron   recurring 5-field cron expression in Europe/Madrid ("0 9 * * 1"), or a descriptor like @daily
  --name   short label (defaults to the start of --text)
  --chat   Telegram chat id to deliver to (default: Jairo's own chat)

Times and cron expressions are always interpreted in Europe/Madrid.
`

// runSched implements the atlas-sched helper binary.
func runSched(args []string) {
	log.SetFlags(0)
	log.SetPrefix("atlas-sched: ")

	if len(args) == 0 {
		fmt.Print(schedUsage)
		os.Exit(2)
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(schedUsage)
	case "list", "ls":
		schedList()
	case "add":
		schedAdd(args[1:])
	case "rm", "delete", "del":
		if len(args) < 2 {
			log.Fatal("rm requires a job id")
		}
		schedRemove(args[1])
	case "run", "run-now":
		if len(args) < 2 {
			log.Fatal("run requires a job id")
		}
		schedRun(args[1])
	default:
		log.Fatalf("unknown command %q (try: list, add, rm, run)", args[0])
	}
}

func schedList() {
	var out struct {
		Timezone string          `json:"timezone"`
		Jobs     []assistant.Job `json:"jobs"`
	}
	if err := apiRequest(http.MethodGet, "/jobs", nil, &out); err != nil {
		log.Fatal(err)
	}
	if len(out.Jobs) == 0 {
		fmt.Println("no jobs scheduled")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tSCHEDULE\tNEXT RUN\tNAME")
	for _, job := range out.Jobs {
		schedule := job.Cron
		if schedule == "" {
			schedule = "once"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			job.ID, job.Type, schedule, job.NextRun.Format("2006-01-02 15:04"), job.Name)
	}
	_ = tw.Flush()
	fmt.Printf("\ntimezone: %s\n", out.Timezone)
}

func schedAdd(args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(schedUsage) }
	jobType := fs.String("type", assistant.JobMessage, "message or agent")
	text := fs.String("text", "", "message text, or prompt for an agent job")
	at := fs.String("at", "", "one-shot time (Europe/Madrid)")
	cronExpr := fs.String("cron", "", "recurring cron expression (Europe/Madrid)")
	name := fs.String("name", "", "short label")
	chat := fs.Int64("chat", 0, "Telegram chat id to deliver to (default: Jairo's own chat)")
	valueFlags := map[string]bool{"type": true, "text": true, "at": true, "cron": true, "name": true, "chat": true}
	if err := fs.Parse(assistant.ReorderFlagsFirst(args, valueFlags)); err != nil {
		log.Fatal(err)
	}

	// Allow the text to be given as trailing positional words too.
	body := strings.TrimSpace(*text)
	if rest := strings.TrimSpace(strings.Join(fs.Args(), " ")); rest != "" {
		if body == "" {
			body = rest
		} else {
			body += " " + rest
		}
	}
	if body == "" {
		log.Fatal("--text is required")
	}
	if *at == "" && *cronExpr == "" {
		log.Fatal("one of --at or --cron is required")
	}
	if *at != "" && *cronExpr != "" {
		log.Fatal("--at and --cron are mutually exclusive")
	}

	job := assistant.Job{Type: *jobType, Payload: body, Name: *name, Cron: *cronExpr, ChatID: *chat}
	if *at != "" {
		when, err := parseWhen(*at)
		if err != nil {
			log.Fatal(err)
		}
		job.At = when
	}

	var created assistant.Job
	if err := apiRequest(http.MethodPost, "/jobs", job, &created); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("created job %s (%s), next run %s\n", created.ID, created.Type, created.NextRun.Format("2006-01-02 15:04 MST"))
}

func schedRemove(id string) {
	if err := apiRequest(http.MethodDelete, "/jobs/"+id, nil, nil); err != nil {
		log.Fatal(err)
	}
	fmt.Println("deleted", id)
}

func schedRun(id string) {
	if err := apiRequest(http.MethodPost, "/jobs/"+id+"/run", nil, nil); err != nil {
		log.Fatal(err)
	}
	fmt.Println("started", id)
}

// parseWhen accepts an absolute time in Europe/Madrid or a relative offset.
func parseWhen(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	loc, err := time.LoadLocation(assistant.ScheduleLocation)
	if err != nil {
		return time.Time{}, err
	}
	if strings.HasPrefix(value, "+") {
		d, err := time.ParseDuration(strings.TrimPrefix(value, "+"))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid relative time %q: %w", value, err)
		}
		return time.Now().In(loc).Add(d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "15:04"} {
		parsed, err := time.ParseInLocation(layout, value, loc)
		if err != nil {
			continue
		}
		if layout == "15:04" {
			// A bare time means today, or tomorrow if it has already passed.
			now := time.Now().In(loc)
			parsed = time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
			if parsed.Before(now) {
				parsed = parsed.Add(24 * time.Hour)
			}
		}
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("could not parse time %q (try \"2006-01-02 15:04\", \"15:04\" or \"+30m\")", value)
}
