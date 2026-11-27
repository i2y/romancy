package romancy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

func capVerdict(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrChannelForbidden):
		return "forbidden"
	case errors.Is(err, ErrNotOwner):
		return "not_owner"
	default:
		return err.Error()
	}
}

// An expired grant keeps its channel sealed (SEMANTICS §3a): the expiry
// sweep never deletes a channel's last grant, so neither the expired token
// nor no token publishes.
func TestCapability_ExpiryDoesNotUnseal(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithTimerCheckInterval(50*time.Millisecond),
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithStaleLockInterval(50*time.Millisecond),
	)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))

	wf := DefineWorkflow("cap_expiry", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		token, _, err := IssueCapability(ctx, storage.CapabilityKindChannelPub, "exp-ch", WithCapabilityTTL(1100*time.Millisecond))
		if err != nil {
			return "", err
		}
		if err := Sleep(ctx, 2500*time.Millisecond); err != nil {
			return "", err
		}
		withToken := Publish(ctx, "exp-ch", "x", WithPublishCapability(token))
		without := Publish(ctx, "exp-ch", "x")
		return fmt.Sprintf("expired-token=%s no-token=%s", capVerdict(withToken), capVerdict(without)), nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	inst := waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status, "output %s", inst.OutputData)
	require.Equal(t, `"expired-token=forbidden no-token=forbidden"`, string(inst.OutputData))
	sealed, err := app.Storage().HasCapabilityGrants(ctx, storage.CapabilityKindChannelPub, "exp-ch")
	require.NoError(t, err)
	require.True(t, sealed, "the sweep deleted the channel's last grant")
	require.Zero(t, countChannelMessages(t, app, "exp-ch"))
}

// Revoking a capability or unregistering a name is the owner's (SEMANTICS
// §3a): another instance is refused with not_owner — a recorded outcome —
// and unknown IDs or unbound names are no-ops.
func TestCapability_OwnerChecks(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	s := app.Storage()
	require.NoError(t, s.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID: "owner-other", WorkflowName: "owner_unregistered", Status: storage.StatusRunning, InputData: []byte(`{}`),
	}))
	require.NoError(t, s.RegisterActorName(ctx, "owned-name", "owner-other", "go"))
	require.NoError(t, s.GrantCapability(ctx, &storage.ActorCapability{
		ID: "cap-owned", Kind: storage.CapabilityKindChannelPub, Target: "owned-ch",
		SecretHash: hashCapabilitySecret("s"), GrantedBy: "owner-other",
	}))

	wf := DefineWorkflow("cap_owner_checks", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		revokeOther := RevokeCapability(ctx, "cap-owned")
		unregOther := UnregisterName(ctx, "owned-name")
		revokeUnknown := RevokeCapability(ctx, "cap-no-such")
		unregUnbound := UnregisterName(ctx, "unbound-name")
		_, capID, err := IssueCapability(ctx, storage.CapabilityKindChannelPub, "mine-ch")
		if err != nil {
			return "", err
		}
		revokeOwn := RevokeCapability(ctx, capID)
		if err := RegisterName(ctx, "mine-name"); err != nil {
			return "", err
		}
		unregOwn := UnregisterName(ctx, "mine-name")
		// The verdicts are read after a replay: refusals come back recorded.
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return "", err
		}
		return fmt.Sprintf("revoke-other=%s unreg-other=%s revoke-unknown=%s unreg-unbound=%s revoke-own=%s unreg-own=%s",
			capVerdict(revokeOther), capVerdict(unregOther), capVerdict(revokeUnknown),
			capVerdict(unregUnbound), capVerdict(revokeOwn), capVerdict(unregOwn)), nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	inst := waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status, "output %s", inst.OutputData)
	require.Equal(t,
		`"revoke-other=not_owner unreg-other=not_owner revoke-unknown=ok unreg-unbound=ok revoke-own=ok unreg-own=ok"`,
		string(inst.OutputData))
	holder, err := s.WhereisActor(ctx, "owned-name")
	require.NoError(t, err)
	require.Equal(t, "owner-other", holder)
	sealed, err := s.HasCapabilityGrants(ctx, storage.CapabilityKindChannelPub, "owned-ch")
	require.NoError(t, err)
	require.True(t, sealed, "another instance revoked the grant")
}

// With a capability key, the token is derived from the capability ID and
// never written to history; a replay derives the same token (SEMANTICS §3a).
func TestCapability_KeyDerivesTokens(t *testing.T) {
	key := []byte("test-capability-key")
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithTimerCheckInterval(50*time.Millisecond),
		WithCapabilityKey(key),
	)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))

	type out struct {
		Token     string `json:"token"`
		CapID     string `json:"cap_id"`
		Published string `json:"published"`
	}
	wf := DefineWorkflow("cap_key_derived", func(ctx *WorkflowContext, _ struct{}) (out, error) {
		token, capID, err := IssueCapability(ctx, storage.CapabilityKindChannelPub, "key-ch")
		if err != nil {
			return out{}, err
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return out{}, err
		}
		return out{Token: token, CapID: capID, Published: capVerdict(Publish(ctx, "key-ch", "x", WithPublishCapability(token)))}, nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	require.NoError(t, err)
	inst := waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status, "output %s", inst.OutputData)
	var got out
	require.NoError(t, json.Unmarshal(inst.OutputData, &got))

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(got.CapID))
	secret := hex.EncodeToString(mac.Sum(nil))
	require.Equal(t, "cap:channel_pub:key-ch:"+secret, got.Token, "the replayed token is not the derived one")
	require.Equal(t, "ok", got.Published)

	events, _, err := app.Storage().GetHistoryPaginated(ctx, id, 0, 100)
	require.NoError(t, err)
	var issued bool
	for _, e := range events {
		if strings.HasPrefix(e.ActivityID, "romancy.issue_capability:") {
			issued = true
			require.True(t, strings.Contains(string(e.EventData), `"derived":true`), "record %s", e.EventData)
		}
		require.False(t, strings.Contains(string(e.EventData), secret), "history holds the secret: %s", e.EventData)
	}
	require.True(t, issued, "the issue step was not recorded")
}

func TestCapability_KeyFromEnvironment(t *testing.T) {
	t.Setenv(capabilityKeyEnv, "env-key")
	require.Equal(t, []byte("env-key"), NewApp(WithDatabase(":memory:")).config.capabilityKey)
	require.Equal(t, []byte("option-key"),
		NewApp(WithDatabase(":memory:"), WithCapabilityKey([]byte("option-key"))).config.capabilityKey)
}

// An incoming CloudEvent for a sealed channel needs a channel_pub token in
// its capabilitytoken extension; otherwise the endpoint answers 403 and
// publishes nothing (SEMANTICS §3a).
func TestCloudEvents_SealedChannelRequiresToken(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	token := grantChannelCap(t, app, storage.CapabilityKindChannelPub, "sealed.event")
	handler := app.Handler()
	post := func(extra string) *httptest.ResponseRecorder {
		body := `{"type":"sealed.event","source":"test","specversion":"1.0","id":"e1"` + extra + `}`
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/cloudevents+json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	for name, extra := range map[string]string{"no token": "", "bad token": `,"capabilitytoken":"cap:channel_pub:sealed.event:wrong"`} {
		w := post(extra)
		require.Equal(t, http.StatusForbidden, w.Code, name)
		var body map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), name)
		require.Equal(t, "Forbidden", body["error_type"], name)
		require.Equal(t, "false", body["retryable"], name)
	}
	require.Zero(t, countChannelMessages(t, app, "sealed.event"), "a refused event was published")

	w := post(`,"capabilitytoken":"` + token + `"`)
	require.Equal(t, http.StatusAccepted, w.Code)
	deadline := time.Now().Add(5 * time.Second)
	for countChannelMessages(t, app, "sealed.event") == 0 {
		require.True(t, time.Now().Before(deadline), "the accepted event was not published")
		time.Sleep(20 * time.Millisecond)
	}
	msgs, err := app.Storage().ChannelMessagesSince(ctx, "sealed.event", "")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.False(t, strings.Contains(string(msgs[0].Data), "capabilitytoken"), "the token was stored with the event: %s", msgs[0].Data)
}
