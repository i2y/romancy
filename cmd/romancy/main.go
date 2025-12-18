// Package main provides a CLI tool for interacting with Romancy workflows.
//
// Usage:
//
//	romancy get <instance_id> [--db <path>]
//	romancy event <instance_id> <event_type> <json_data> [--db <path>] [--url <base_url>]
//	romancy list [--db <path>] [--status <status>] [--page-token <token>]
//	romancy cancel <instance_id> [--db <path>] [--url <base_url>]
//
// Note: Database migrations are handled by dbmate. See schema/db/migrations/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

var (
	dbPath  string
	baseURL string
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	cmdArgs := os.Args[2:]

	switch cmd {
	case "get":
		fs := flag.NewFlagSet("get", flag.ExitOnError)
		fs.StringVar(&dbPath, "db", "romancy.db", "Path to database")
		_ = fs.Parse(cmdArgs)
		args := fs.Args()

		if len(args) < 1 {
			fmt.Println("Error: instance_id is required")
			fmt.Println("Usage: romancy get <instance_id> [--db <path>]")
			os.Exit(1)
		}
		if err := cmdGet(args[0]); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "event":
		fs := flag.NewFlagSet("event", flag.ExitOnError)
		fs.StringVar(&dbPath, "db", "romancy.db", "Path to database")
		fs.StringVar(&baseURL, "url", "http://localhost:8080", "Base URL of running Romancy server")
		_ = fs.Parse(cmdArgs)
		args := fs.Args()

		if len(args) < 3 {
			fmt.Println("Error: instance_id, event_type, and json_data are required")
			fmt.Println("Usage: romancy event <instance_id> <event_type> <json_data> [--db <path>] [--url <base_url>]")
			os.Exit(1)
		}
		if err := cmdEvent(args[0], args[1], args[2]); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		fs.StringVar(&dbPath, "db", "romancy.db", "Path to database")
		statusFlag := fs.String("status", "", "Filter by status")
		pageToken := fs.String("page-token", "", "Pagination token for next page")
		_ = fs.Parse(cmdArgs)

		if err := cmdList(*statusFlag, *pageToken); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "cancel":
		fs := flag.NewFlagSet("cancel", flag.ExitOnError)
		fs.StringVar(&dbPath, "db", "romancy.db", "Path to database")
		fs.StringVar(&baseURL, "url", "http://localhost:8080", "Base URL of running Romancy server")
		_ = fs.Parse(cmdArgs)
		args := fs.Args()

		if len(args) < 1 {
			fmt.Println("Error: instance_id is required")
			fmt.Println("Usage: romancy cancel <instance_id> [--db <path>]")
			os.Exit(1)
		}
		if err := cmdCancel(args[0]); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "migrate":
		fmt.Println("The 'migrate' command has been removed.")
		fmt.Println("Please use dbmate for migrations:")
		fmt.Println("  dbmate -d schema/db/migrations/sqlite up")
		fmt.Println("  dbmate -d schema/db/migrations/postgresql up")
		fmt.Println("  dbmate -d schema/db/migrations/mysql up")
		os.Exit(1)

	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Romancy CLI - Interact with Romancy workflows")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  romancy <command> [arguments] [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  get <instance_id>                     Get workflow instance details")
	fmt.Println("  event <instance_id> <type> <json>     Send an event to a workflow")
	fmt.Println("  list                                  List workflow instances")
	fmt.Println("  cancel <instance_id>                  Cancel a workflow")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --db <path>         Database path/URL (default: romancy.db)")
	fmt.Println("  --url <base_url>    Romancy server URL (default: http://localhost:8080)")
	fmt.Println("  --status <status>   Filter by workflow status (for list command)")
	fmt.Println("  --page-token <tok>  Pagination token for next page (for list command)")
	fmt.Println()
	fmt.Println("Status Values:")
	fmt.Println("  pending, running, completed, failed, canceled,")
	fmt.Println("  waiting_event, waiting_timer, waiting_message, recurred, compensating")
	fmt.Println()
	fmt.Println("Database Migrations:")
	fmt.Println("  Use dbmate for database migrations:")
	fmt.Println("  dbmate -d schema/db/migrations/sqlite up")
	fmt.Println("  dbmate -d schema/db/migrations/postgresql up")
	fmt.Println("  dbmate -d schema/db/migrations/mysql up")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  romancy get wf-123abc")
	fmt.Println("  romancy event wf-123abc payment.completed '{\"transaction_id\":\"TX-001\"}'")
	fmt.Println("  romancy list --status running")
	fmt.Println("  romancy list --page-token 'eyJ...'")
	fmt.Println("  romancy cancel wf-123abc")
}

func openStorage() (storage.Storage, error) {
	store, err := storage.NewSQLiteStorage(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return store, nil
}

// cmdGet displays detailed information about a workflow instance.
func cmdGet(instanceID string) error {
	store, err := openStorage()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	instance, err := store.GetInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}
	if instance == nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}

	fmt.Println("=== Workflow Instance ===")
	fmt.Printf("Instance ID:  %s\n", instance.InstanceID)
	fmt.Printf("Workflow:     %s\n", instance.WorkflowName)
	fmt.Printf("Status:       %s\n", statusEmoji(instance.Status))
	fmt.Printf("Activity ID:  %s\n", instance.CurrentActivityID)
	fmt.Printf("Started:      %s\n", instance.StartedAt.Format(time.RFC3339))
	fmt.Printf("Updated:      %s\n", instance.UpdatedAt.Format(time.RFC3339))

	if instance.LockedBy != "" {
		fmt.Printf("Locked By:    %s\n", instance.LockedBy)
		if instance.LockedAt != nil {
			fmt.Printf("Locked At:    %s\n", instance.LockedAt.Format(time.RFC3339))
		}
	}

	// Display input
	if len(instance.InputData) > 0 {
		fmt.Println()
		fmt.Println("--- Input ---")
		prettyPrintJSON(instance.InputData)
	}

	// Display output
	if len(instance.OutputData) > 0 {
		fmt.Println()
		fmt.Println("--- Output ---")
		prettyPrintJSON(instance.OutputData)
	}

	// Get and display history using iterator
	iter := storage.NewHistoryIterator(ctx, store, instanceID, nil)
	defer func() { _ = iter.Close() }()

	fmt.Println()
	fmt.Println("--- History ---")
	i := 0
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		i++
		fmt.Printf("%d. [%s] %s - %s\n",
			i,
			event.CreatedAt.Format("15:04:05"),
			event.EventType,
			event.ActivityID,
		)
	}
	if err := iter.Err(); err != nil {
		fmt.Printf("\nWarning: failed to get history: %v\n", err)
	}
	if i == 0 {
		fmt.Println("(no history)")
	}

	return nil
}

// cmdEvent sends a CloudEvent to a running Romancy server.
func cmdEvent(instanceID, eventType, jsonData string) error {
	// Validate JSON
	var data interface{}
	if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
		return fmt.Errorf("invalid JSON data: %w", err)
	}

	// Create CloudEvent
	cloudEvent := map[string]interface{}{
		"specversion":     "1.0",
		"type":            eventType,
		"source":          "romancy-cli",
		"id":              fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		"time":            time.Now().UTC().Format(time.RFC3339),
		"datacontenttype": "application/json",
		"data":            data,
		// Extension for targeting specific instance
		"romancyinstanceid": instanceID,
	}

	body, err := json.Marshal(cloudEvent)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	// Send HTTP request
	resp, err := http.Post(baseURL+"/", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to send event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned error %d: %s", resp.StatusCode, string(respBody))
	}

	fmt.Printf("✓ Event sent successfully\n")
	fmt.Printf("  Instance:   %s\n", instanceID)
	fmt.Printf("  Event Type: %s\n", eventType)
	fmt.Printf("  Response:   %s\n", string(respBody))

	return nil
}

// cmdList lists workflow instances with optional status filtering and pagination.
func cmdList(statusFilter, pageToken string) error {
	store, err := openStorage()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	opts := storage.ListInstancesOptions{
		Limit:     50,
		PageToken: pageToken,
	}

	if statusFilter != "" {
		opts.StatusFilter = parseStatus(statusFilter)
	}

	result, err := store.ListInstances(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	if len(result.Instances) == 0 {
		fmt.Println("No workflow instances found.")
		return nil
	}

	fmt.Printf("%-40s %-20s %-15s %-20s\n", "INSTANCE ID", "WORKFLOW", "STATUS", "UPDATED")
	fmt.Println(strings.Repeat("-", 100))

	for _, inst := range result.Instances {
		fmt.Printf("%-40s %-20s %-15s %-20s\n",
			truncate(inst.InstanceID, 38),
			truncate(inst.WorkflowName, 18),
			statusEmoji(inst.Status),
			inst.UpdatedAt.Format("2006-01-02 15:04:05"),
		)
	}

	fmt.Printf("\nTotal: %d instances\n", len(result.Instances))
	if result.HasMore {
		fmt.Printf("(More results available, use --page-token '%s' for next page)\n", result.NextPageToken)
	}

	return nil
}

// cmdCancel cancels a workflow by sending a cancel request to the server.
func cmdCancel(instanceID string) error {
	url := fmt.Sprintf("%s/cancel/%s", baseURL, instanceID)

	req, err := http.NewRequest(http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send cancel request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned error %d: %s", resp.StatusCode, string(respBody))
	}

	fmt.Printf("✓ Workflow canceled: %s\n", instanceID)

	return nil
}

// Helper functions

func statusEmoji(status storage.WorkflowStatus) string {
	switch status {
	case storage.StatusRunning:
		return "🏃 running"
	case storage.StatusWaitingForEvent:
		return "⏸️  waiting_event"
	case storage.StatusWaitingForTimer:
		return "⏰ waiting_timer"
	case storage.StatusWaitingForMessage:
		return "📨 waiting_message"
	case storage.StatusCompleted:
		return "✅ completed"
	case storage.StatusFailed:
		return "❌ failed"
	case storage.StatusCancelled:
		return "🚫 canceled"
	case storage.StatusRecurred:
		return "🔄 recurred"
	case storage.StatusCompensating:
		return "⏪ compensating"
	default:
		return string(status)
	}
}

func parseStatus(s string) storage.WorkflowStatus {
	s = strings.ToLower(s)
	switch s {
	case "running":
		return storage.StatusRunning
	case "waiting", "waiting_event", "waiting_for_event":
		return storage.StatusWaitingForEvent
	case "waiting_timer", "waiting_for_timer":
		return storage.StatusWaitingForTimer
	case "waiting_message", "waiting_for_message":
		return storage.StatusWaitingForMessage
	case "completed":
		return storage.StatusCompleted
	case "failed":
		return storage.StatusFailed
	case "canceled":
		return storage.StatusCancelled
	case "recurred":
		return storage.StatusRecurred
	case "compensating":
		return storage.StatusCompensating
	default:
		return storage.WorkflowStatus(s)
	}
}

func prettyPrintJSON(data []byte) {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		// Fall back to raw output
		fmt.Println(string(data))
		return
	}
	fmt.Println(out.String())
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
