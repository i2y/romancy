package romancy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// grantChannelCap issues a capability grant directly in storage and returns
// the full token form cap:{kind}:{target}:{secret}.
func grantChannelCap(t *testing.T, app *App, kind, target string) string {
	t.Helper()
	secret := "test-secret-" + target
	capID := fmt.Sprintf("cap-test-%s-%s", kind, target)
	err := app.Storage().GrantCapability(context.Background(), &storage.ActorCapability{
		ID:         capID,
		Kind:       kind,
		Target:     target,
		SecretHash: hashCapabilitySecret(secret),
		GrantedBy:  "test",
	})
	require.NoError(t, err)
	return fmt.Sprintf("cap:%s:%s:%s", kind, target, secret)
}

func runCapWorkflow(t *testing.T, app *App, wf *WorkflowFunc[json.RawMessage, string], input string) string {
	t.Helper()
	ctx := context.Background()
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, json.RawMessage(input))
	require.NoError(t, err)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, id)
		require.NoError(t, err)
		if inst != nil && (inst.Status == storage.StatusCompleted || inst.Status == storage.StatusFailed) {
			require.NotNil(t, inst.OutputData, "workflow finished without output")
			var out string
			require.NoError(t, json.Unmarshal(inst.OutputData, &out))
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("workflow did not finish in time")
	return ""
}

func pubWorkflow(name string) *WorkflowFunc[json.RawMessage, string] {
	return DefineWorkflow(name, func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		var in struct {
			Token   string `json:"token"`
			Channel string `json:"channel"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		err := Publish(ctx, in.Channel, "hello", WithPublishCapability(in.Token))
		if err != nil {
			return "err:" + err.Error(), nil
		}
		return "ok", nil
	})
}

func recvWorkflow(name string) *WorkflowFunc[json.RawMessage, string] {
	return DefineWorkflow(name, func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		var in struct {
			Token   string `json:"token"`
			Channel string `json:"channel"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		if err := Subscribe(ctx, in.Channel, ModeBroadcast); err != nil {
			return "", err
		}
		// No publisher: a permission error must surface BEFORE any blocking.
		_, err := Receive[json.RawMessage](ctx, in.Channel, WithReceiveCapability(in.Token), WithReceiveTimeout(5*time.Second))
		if err != nil {
			return "err:" + err.Error(), nil
		}
		return "ok", nil
	})
}

func TestChannelCapability_OpenChannelAllowsAnonymousPublish(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	// no grants for "open-ch" — it stays open
	out := runCapWorkflow(t, app, pubWorkflow("cap-pub-open"), `{"channel":"open-ch"}`)
	assert.Equal(t, "ok", out)
}

func TestChannelCapability_SealedPublishRequiresToken(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	token := grantChannelCap(t, app, storage.CapabilityKindChannelPub, "sealed-pub-ch")

	// without a token the publish is rejected
	out := runCapWorkflow(t, app, pubWorkflow("cap-pub-denied"), `{"channel":"sealed-pub-ch"}`)
	assert.Contains(t, out, "permission denied")

	// a wrong token is rejected
	out = runCapWorkflow(t, app, pubWorkflow("cap-pub-wrong"), `{"channel":"sealed-pub-ch","token":"cap:channel_pub:sealed-pub-ch:wrong"}`)
	assert.Contains(t, out, "permission denied")

	// the valid token passes
	out = runCapWorkflow(t, app, pubWorkflow("cap-pub-ok"), fmt.Sprintf(`{"channel":"sealed-pub-ch","token":%q}`, token))
	assert.Equal(t, "ok", out)
}

func TestChannelCapability_SealedReceiveRequiresToken(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	_ = grantChannelCap(t, app, storage.CapabilityKindChannelSub, "sealed-sub-ch")

	// without a token the receive is rejected before blocking
	out := runCapWorkflow(t, app, recvWorkflow("cap-recv-denied"), `{"channel":"sealed-sub-ch"}`)
	assert.Contains(t, out, "permission denied")
}

func TestChannelCapability_UnsealedSubStaysOpen(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	// sealing ONE channel must not affect others
	_ = grantChannelCap(t, app, storage.CapabilityKindChannelSub, "other-ch")

	// "plain-ch" has no grants: the receive proceeds past enforcement and
	// blocks until the timeout — proof is the timeout error, not permission.
	out := runCapWorkflow(t, app, recvWorkflow("cap-recv-open"), `{"channel":"plain-ch"}`)
	assert.Contains(t, out, "timeout")
}
