---
title: "E-commerce Example"
weight: 2
---

This example demonstrates type-safe workflows using Go structs with validation.

## What This Example Shows

- ✅ Struct-based parameters with compile-time type safety
- ✅ Nested structs (`Customer`, `Address`, `OrderItem`)
- ✅ Struct-based return values
- ✅ Type restoration during workflow execution
- ✅ JSON storage of structs

## Code Overview

### Define Structs

```go
package main

import (
	"fmt"
	"regexp"
	"time"
)

// Address represents a customer's address
type Address struct {
	Street  string `json:"street"`
	City    string `json:"city"`
	State   string `json:"state"`
	ZipCode string `json:"zip_code"`
}

// Validate validates the address
func (a Address) Validate() error {
	zipPattern := regexp.MustCompile(`^\d{5}(-\d{4})?$`)
	if !zipPattern.MatchString(a.ZipCode) {
		return fmt.Errorf("invalid zip code: %s", a.ZipCode)
	}
	return nil
}

// Customer represents a customer
type Customer struct {
	CustomerID string   `json:"customer_id"`
	Name       string   `json:"name"`
	Email      string   `json:"email"`
	Phone      string   `json:"phone,omitempty"`
	Address    Address  `json:"address"`
}

// Validate validates the customer
func (c Customer) Validate() error {
	customerPattern := regexp.MustCompile(`^CUST-\d+$`)
	if !customerPattern.MatchString(c.CustomerID) {
		return fmt.Errorf("invalid customer ID: %s", c.CustomerID)
	}
	if c.Name == "" || len(c.Name) > 100 {
		return fmt.Errorf("name must be 1-100 characters")
	}
	emailPattern := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	if !emailPattern.MatchString(c.Email) {
		return fmt.Errorf("invalid email: %s", c.Email)
	}
	return c.Address.Validate()
}

// OrderItem represents an item in an order
type OrderItem struct {
	ProductID string  `json:"product_id"`
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
}

// Subtotal calculates the item subtotal
func (i OrderItem) Subtotal() float64 {
	return float64(i.Quantity) * i.UnitPrice
}

// Validate validates the order item
func (i OrderItem) Validate() error {
	productPattern := regexp.MustCompile(`^PROD-\d+$`)
	if !productPattern.MatchString(i.ProductID) {
		return fmt.Errorf("invalid product ID: %s", i.ProductID)
	}
	if i.Quantity < 1 || i.Quantity > 100 {
		return fmt.Errorf("quantity must be 1-100")
	}
	if i.UnitPrice < 0.01 {
		return fmt.Errorf("unit price must be >= 0.01")
	}
	return nil
}

// OrderInput represents the workflow input
type OrderInput struct {
	OrderID  string      `json:"order_id"`
	Customer Customer    `json:"customer"`
	Items    []OrderItem `json:"items"`
	Priority string      `json:"priority"`
	Notes    string      `json:"notes,omitempty"`
}

// Validate validates the order input
func (o OrderInput) Validate() error {
	orderPattern := regexp.MustCompile(`^ORD-\d+$`)
	if !orderPattern.MatchString(o.OrderID) {
		return fmt.Errorf("invalid order ID: %s", o.OrderID)
	}
	if err := o.Customer.Validate(); err != nil {
		return fmt.Errorf("invalid customer: %w", err)
	}
	if len(o.Items) == 0 {
		return fmt.Errorf("order must have at least one item")
	}
	for _, item := range o.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("invalid item: %w", err)
		}
	}
	validPriorities := map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}
	if !validPriorities[o.Priority] {
		return fmt.Errorf("invalid priority: %s", o.Priority)
	}
	return nil
}

// TotalAmount calculates the total order amount
func (o OrderInput) TotalAmount() float64 {
	total := 0.0
	for _, item := range o.Items {
		total += item.Subtotal()
	}
	return total
}

// OrderResult represents the workflow result
type OrderResult struct {
	OrderID            string     `json:"order_id"`
	Status             string     `json:"status"`
	ConfirmationNumber string     `json:"confirmation_number"`
	TotalAmount        float64    `json:"total_amount"`
	EstimatedDelivery  *time.Time `json:"estimated_delivery,omitempty"`
	Message            string     `json:"message"`
}
```

### Define Workflow

```go
package main

import (
	"fmt"

	"github.com/i2y/romancy"
)

// processOrder is an e-commerce order processing workflow with type-safe structs.
// Romancy automatically:
// - Stores order as JSON in the database
// - Restores OrderInput type on replay
// - Returns OrderResult with proper typing
var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		// Validate input
		if err := order.Validate(); err != nil {
			return OrderResult{}, fmt.Errorf("validation failed: %w", err)
		}

		// Calculate total from items
		totalAmount := order.TotalAmount()

		fmt.Printf("Processing order %s\n", order.OrderID)
		fmt.Printf("Customer: %s (%s)\n", order.Customer.Name, order.Customer.Email)
		fmt.Printf("Items: %d, Total: $%.2f\n", len(order.Items), totalAmount)

		// Workflow logic here...

		return OrderResult{
			OrderID:            order.OrderID,
			Status:             "completed",
			ConfirmationNumber: fmt.Sprintf("CONF-%s", order.OrderID),
			TotalAmount:        totalAmount,
			Message:            fmt.Sprintf("Order processed for %s", order.Customer.Name),
		}, nil
	},
)
```

### Start the Workflow

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
)

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("order-worker"),
	)

	ctx := context.Background()

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Create order with nested structs
	order := OrderInput{
		OrderID: "ORD-12345",
		Customer: Customer{
			CustomerID: "CUST-001",
			Name:       "Alice Johnson",
			Email:      "alice@example.com",
			Address: Address{
				Street:  "123 Main St",
				City:    "Springfield",
				State:   "IL",
				ZipCode: "62701",
			},
		},
		Items: []OrderItem{
			{ProductID: "PROD-101", Quantity: 2, UnitPrice: 29.99},
			{ProductID: "PROD-202", Quantity: 1, UnitPrice: 49.99},
		},
		Priority: "high",
	}

	// Start workflow - validation happens in workflow
	instanceID, err := romancy.StartWorkflow(ctx, app, processOrder, order)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Order started: %s\n", instanceID)
}
```

## Benefits of Struct-Based Workflows

### 1. Compile-Time Type Safety

```go
// This won't compile - type safety at build time
var badWorkflow = romancy.DefineWorkflow("bad",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		// order.Customer.Name is string ✅
		// order.Items[0].Quantity is int ✅
		return OrderResult{}, nil
	},
)
```

### 2. Explicit Validation

```go
// Validation with clear error messages
func (o OrderInput) Validate() error {
	if !orderPattern.MatchString(o.OrderID) {
		return fmt.Errorf("invalid order ID: %s", o.OrderID)
	}
	// ... more validation
	return nil
}
```

### 3. IDE Support

```go
var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		// IDE autocomplete works!
		customerName := order.Customer.Name  // ✅ Type-safe
		total := order.TotalAmount()         // ✅ Type-safe
		return OrderResult{...}, nil         // ✅ Return type checked
	},
)
```

### 4. JSON Storage with Type Restoration

Romancy stores structs as JSON and automatically restores them:

```go
// First run: OrderInput → JSON → Database
// Replay: Database → JSON → OrderInput (automatic restoration)
```

## Using a Validation Library

For more sophisticated validation, use a validation library like `go-playground/validator`:

```go
package main

import (
	"github.com/go-playground/validator/v10"
)

// OrderInputWithTags uses struct tags for validation
type OrderInputWithTags struct {
	OrderID  string      `json:"order_id" validate:"required,startswith=ORD-"`
	Customer Customer    `json:"customer" validate:"required"`
	Items    []OrderItem `json:"items" validate:"required,min=1,dive"`
	Priority string      `json:"priority" validate:"required,oneof=low normal high urgent"`
	Notes    string      `json:"notes,omitempty"`
}

var validate = validator.New()

func (o OrderInputWithTags) Validate() error {
	return validate.Struct(o)
}
```

## Running the Example

Create a file named `ecommerce_workflow.go` with the structs and workflow shown above, then run:

```bash
# Initialize Go module
go mod init ecommerce-example
go get github.com/i2y/romancy

# Run your workflow
go run ecommerce_workflow.go
```

## Complete Code

```go
package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/i2y/romancy"
)

// Address represents a customer's address
type Address struct {
	Street  string `json:"street"`
	City    string `json:"city"`
	State   string `json:"state"`
	ZipCode string `json:"zip_code"`
}

func (a Address) Validate() error {
	zipPattern := regexp.MustCompile(`^\d{5}(-\d{4})?$`)
	if !zipPattern.MatchString(a.ZipCode) {
		return fmt.Errorf("invalid zip code: %s", a.ZipCode)
	}
	return nil
}

// Customer represents a customer
type Customer struct {
	CustomerID string  `json:"customer_id"`
	Name       string  `json:"name"`
	Email      string  `json:"email"`
	Phone      string  `json:"phone,omitempty"`
	Address    Address `json:"address"`
}

func (c Customer) Validate() error {
	customerPattern := regexp.MustCompile(`^CUST-\d+$`)
	if !customerPattern.MatchString(c.CustomerID) {
		return fmt.Errorf("invalid customer ID: %s", c.CustomerID)
	}
	if c.Name == "" || len(c.Name) > 100 {
		return fmt.Errorf("name must be 1-100 characters")
	}
	emailPattern := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	if !emailPattern.MatchString(c.Email) {
		return fmt.Errorf("invalid email: %s", c.Email)
	}
	return c.Address.Validate()
}

// OrderItem represents an item in an order
type OrderItem struct {
	ProductID string  `json:"product_id"`
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
}

func (i OrderItem) Subtotal() float64 {
	return float64(i.Quantity) * i.UnitPrice
}

func (i OrderItem) Validate() error {
	productPattern := regexp.MustCompile(`^PROD-\d+$`)
	if !productPattern.MatchString(i.ProductID) {
		return fmt.Errorf("invalid product ID: %s", i.ProductID)
	}
	if i.Quantity < 1 || i.Quantity > 100 {
		return fmt.Errorf("quantity must be 1-100")
	}
	if i.UnitPrice < 0.01 {
		return fmt.Errorf("unit price must be >= 0.01")
	}
	return nil
}

// OrderInput represents the workflow input
type OrderInput struct {
	OrderID  string      `json:"order_id"`
	Customer Customer    `json:"customer"`
	Items    []OrderItem `json:"items"`
	Priority string      `json:"priority"`
	Notes    string      `json:"notes,omitempty"`
}

func (o OrderInput) Validate() error {
	orderPattern := regexp.MustCompile(`^ORD-\d+$`)
	if !orderPattern.MatchString(o.OrderID) {
		return fmt.Errorf("invalid order ID: %s", o.OrderID)
	}
	if err := o.Customer.Validate(); err != nil {
		return fmt.Errorf("invalid customer: %w", err)
	}
	if len(o.Items) == 0 {
		return fmt.Errorf("order must have at least one item")
	}
	for _, item := range o.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("invalid item: %w", err)
		}
	}
	validPriorities := map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}
	if !validPriorities[o.Priority] {
		return fmt.Errorf("invalid priority: %s", o.Priority)
	}
	return nil
}

func (o OrderInput) TotalAmount() float64 {
	total := 0.0
	for _, item := range o.Items {
		total += item.Subtotal()
	}
	return total
}

// OrderResult represents the workflow result
type OrderResult struct {
	OrderID            string     `json:"order_id"`
	Status             string     `json:"status"`
	ConfirmationNumber string     `json:"confirmation_number"`
	TotalAmount        float64    `json:"total_amount"`
	EstimatedDelivery  *time.Time `json:"estimated_delivery,omitempty"`
	Message            string     `json:"message"`
}

// Workflow

var processOrder = romancy.DefineWorkflow("process_order",
	func(ctx *romancy.WorkflowContext, order OrderInput) (OrderResult, error) {
		if err := order.Validate(); err != nil {
			return OrderResult{}, fmt.Errorf("validation failed: %w", err)
		}

		totalAmount := order.TotalAmount()

		fmt.Printf("Processing order %s\n", order.OrderID)
		fmt.Printf("Customer: %s (%s)\n", order.Customer.Name, order.Customer.Email)
		fmt.Printf("Items: %d, Total: $%.2f\n", len(order.Items), totalAmount)

		return OrderResult{
			OrderID:            order.OrderID,
			Status:             "completed",
			ConfirmationNumber: fmt.Sprintf("CONF-%s", order.OrderID),
			TotalAmount:        totalAmount,
			Message:            fmt.Sprintf("Order processed for %s", order.Customer.Name),
		}, nil
	},
)

func main() {
	fmt.Println("============================================================")
	fmt.Println("Romancy Framework - E-commerce Workflow Example")
	fmt.Println("============================================================")
	fmt.Println()

	app := romancy.NewApp(
		romancy.WithDatabase("orders.db"),
		romancy.WithWorkerID("order-worker"),
	)

	ctx := context.Background()

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	order := OrderInput{
		OrderID: "ORD-12345",
		Customer: Customer{
			CustomerID: "CUST-001",
			Name:       "Alice Johnson",
			Email:      "alice@example.com",
			Address: Address{
				Street:  "123 Main St",
				City:    "Springfield",
				State:   "IL",
				ZipCode: "62701",
			},
		},
		Items: []OrderItem{
			{ProductID: "PROD-101", Quantity: 2, UnitPrice: 29.99},
			{ProductID: "PROD-202", Quantity: 1, UnitPrice: 49.99},
		},
		Priority: "high",
	}

	fmt.Println(">>> Starting order workflow...")
	fmt.Println()

	instanceID, err := romancy.StartWorkflow(ctx, app, processOrder, order)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n>>> Order started with instance ID: %s\n", instanceID)
}
```

## What You Learned

- ✅ **Go Structs** provide compile-time type safety
- ✅ **Nested Structs** work seamlessly with Romancy
- ✅ **Explicit Validation** with Validate() methods
- ✅ **Type Restoration** works during replay
- ✅ **JSON Storage** handles complex struct hierarchies

## Next Steps

- **[Saga Pattern](/docs/examples/saga)**: Add compensation to this workflow
- **[Your First Workflow](/docs/getting-started/first-workflow)**: Step-by-step order processing tutorial
