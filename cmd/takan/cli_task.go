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

// taskUsage documents the atlas-task helper. This is how the CLI agent keeps
// the Telegram conversation responsive: anything slow goes here.
const taskUsage = `atlas-task — run long work in the background, off the conversation.

Usage:
  atlas-task run "<prompt>" [--title "<short label>"] [--chat <chat id>]
  atlas-task list
  atlas-task status <id>
  atlas-task kill <id>

"run" returns a task id immediately. The task executes in its own fresh agent
session under workspace/tasks/<id>/, and its output is delivered to Jairo's
Telegram when it finishes. Use it for anything that plausibly takes more than a
minute or two: builds, research, multi-step operations, launching agents on
other machines.
`

// runTask implements the atlas-task helper binary.
func runTask(args []string) {
	log.SetFlags(0)
	log.SetPrefix("atlas-task: ")

	if len(args) == 0 {
		fmt.Print(taskUsage)
		os.Exit(2)
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(taskUsage)
	case "run", "start":
		taskRun(args[1:])
	case "list", "ls":
		taskList()
	case "status", "show", "get":
		if len(args) < 2 {
			log.Fatal("status requires a task id")
		}
		taskStatus(args[1])
	case "kill", "stop", "cancel":
		if len(args) < 2 {
			log.Fatal("kill requires a task id")
		}
		taskKill(args[1])
	default:
		log.Fatalf("unknown command %q (try: run, list, status, kill)", args[0])
	}
}

func taskRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(taskUsage) }
	title := fs.String("title", "", "short label for the task")
	chat := fs.Int64("chat", 0, "Telegram chat id to deliver the result to (default: Jairo's own chat)")
	prompt := fs.String("prompt", "", "the prompt (may also be given positionally)")
	if err := fs.Parse(assistant.ReorderFlagsFirst(args, map[string]bool{"title": true, "prompt": true, "chat": true})); err != nil {
		log.Fatal(err)
	}

	body := strings.TrimSpace(*prompt)
	if rest := strings.TrimSpace(strings.Join(fs.Args(), " ")); rest != "" {
		if body == "" {
			body = rest
		} else {
			body += " " + rest
		}
	}
	if body == "" {
		log.Fatal("a prompt is required")
	}

	var task assistant.Task
	if err := apiRequest(http.MethodPost, "/tasks",
		map[string]any{"prompt": body, "title": *title, "chat_id": *chat}, &task); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("started task %s (%s)\nthe result will be sent to Jairo's Telegram when it finishes\n", task.ID, task.Title)
}

func taskList() {
	var out struct {
		Tasks []assistant.Task `json:"tasks"`
	}
	if err := apiRequest(http.MethodGet, "/tasks", nil, &out); err != nil {
		log.Fatal(err)
	}
	if len(out.Tasks) == 0 {
		fmt.Println("no background tasks")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tDURATION\tSTARTED\tTITLE")
	for _, t := range out.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.State, t.Duration().Truncate(time.Second), t.StartedAt.Format("15:04:05"), t.Title)
	}
	_ = tw.Flush()
}

func taskStatus(id string) {
	var task assistant.Task
	if err := apiRequest(http.MethodGet, "/tasks/"+id, nil, &task); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("id:       %s\n", task.ID)
	fmt.Printf("title:    %s\n", task.Title)
	fmt.Printf("state:    %s\n", task.State)
	fmt.Printf("duration: %s\n", task.Duration().Truncate(time.Second))
	fmt.Printf("started:  %s\n", task.StartedAt.Format(time.RFC3339))
	if !task.FinishedAt.IsZero() {
		fmt.Printf("finished: %s\n", task.FinishedAt.Format(time.RFC3339))
	}
	if task.PID != 0 {
		fmt.Printf("pid:      %d\n", task.PID)
	}
	if task.Error != "" {
		fmt.Printf("error:    %s\n", task.Error)
	}
	fmt.Printf("dir:      %s\n", task.Dir)
	if task.OutputPath != "" && !task.Running() {
		fmt.Printf("output:   %s\n", task.OutputPath)
	}
}

func taskKill(id string) {
	var out struct {
		Killed bool `json:"killed"`
	}
	if err := apiRequest(http.MethodDelete, "/tasks/"+id, nil, &out); err != nil {
		log.Fatal(err)
	}
	if out.Killed {
		fmt.Println("killed", id)
		return
	}
	fmt.Println(id, "was not running")
}
