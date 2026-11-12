//go:build integration

package cv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/antivirus"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/storage"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
)

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// pipeline is a real worker wired to real NATS, Postgres and MinIO.
type pipeline struct {
	db      *postgres.DB
	bus     *events.Bus
	store   *storage.S3Store
	runtime *events.Runtime
	repo    *cv.PostgresRepository
}

// startPipeline brings up the cv consumers against the live development stack.
//
// Deliberately the real Runtime rather than calling handlers directly: the properties
// MJF-007 asks about — ordering, idempotency across redelivery, dead-lettering — are
// produced by the runtime's interaction with JetStream and the claim store, so a test
// that invoked handlers itself would exercise none of them.
func startPipeline(t *testing.T) *pipeline {
	t.Helper()

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		t.Skip("NATS_URL not set; run with the development stack up")
	}
	if os.Getenv("STORAGE_ENDPOINT") == "" {
		t.Skip("STORAGE_ENDPOINT not set; run with the development stack up")
	}

	db := openDB(t)
	var logOut io.Writer = io.Discard
	if os.Getenv("PIPELINE_TEST_LOG") != "" {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()

	store, err := storage.Open(ctx, config.StorageConfig{
		Endpoint:  os.Getenv("STORAGE_ENDPOINT"),
		AccessKey: os.Getenv("STORAGE_ACCESS_KEY"),
		SecretKey: os.Getenv("STORAGE_SECRET_KEY"),
		Bucket:    os.Getenv("STORAGE_BUCKET"),
		Region:    "us-east-1",
		UseSSL:    false,
		// Without this the limit is zero and every document is rejected as too large.
		MaxCVBytes: 10 << 20,
	})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	bus, err := events.Connect(ctx, config.NATSConfig{
		URL: natsURL, Name: "cv-pipeline-test", MaxReconnects: 3,
		ReconnectWait: time.Second, StreamReplicas: 1,
	}, log)
	if err != nil {
		t.Fatalf("connect nats: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	repo := cv.NewPostgresRepository(db)
	worker := cv.NewWorker(cv.WorkerDeps{
		Repo:    repo,
		Store:   store,
		Scanner: antivirus.NewDevelopmentScanner(log),
		// No extractor: without AI credentials the structure stage is skipped and a
		// version is marked parsed on extracted text alone. That is the supported
		// degraded mode, and it is enough to carry a CV from uploaded to parsed.
		Extractor: nil,
		Embedder:  nil,
		Notifier:  nil,
		Bus:       bus,
		Logger:    log,
		Limits:    docextract.DefaultLimits(),
	})

	idempotency := events.NewPostgresIdempotencyStore(db, 45*24*time.Hour)
	runtime := events.NewRuntime(bus, idempotency, log, 30*time.Second, 3)

	if err := runtime.Start(ctx, worker.Registrations(), []string{"cv"}); err != nil {
		t.Fatalf("start consumers: %v", err)
	}
	t.Cleanup(func() { runtime.Stop(10 * time.Second) })

	return &pipeline{db: db, bus: bus, store: store, runtime: runtime, repo: repo}
}

// uploadCV puts a real document in object storage and creates the rows an upload would.
func (p *pipeline) uploadCV(t *testing.T, content []byte) (userID, documentID, versionID uuid.UUID, storageKey string) {
	t.Helper()
	ctx := context.Background()

	userID, documentID, versionID = uuid.New(), uuid.New(), uuid.New()

	obj, err := p.store.Put(ctx, userID, docxType, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put object: %v", err)
	}
	storageKey = obj.Key

	q := p.db.Querier(ctx)
	_, err = q.Exec(ctx, `INSERT INTO users (id, email, password_hash, email_verified_at)
		VALUES ($1, $2, 'x', now())`, userID, "pipeline-"+userID.String()[:8]+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := q.Exec(ctx, `INSERT INTO candidate_profiles (user_id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	_, err = q.Exec(ctx, `INSERT INTO cv_documents (id, user_id, content_type, size_bytes)
		VALUES ($1, $2, $3, $4)`, documentID, userID, docxType, len(content))
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	_, err = q.Exec(ctx, `INSERT INTO cv_versions
		(id, cv_document_id, user_id, version_no, storage_key, checksum_sha256, size_bytes)
		VALUES ($1, $2, $3, 1, $4, $5, $6)`,
		// A slice, not the fixed-size array: pgx has no encode plan for [32]byte -> bytea.
		versionID, documentID, userID, storageKey, obj.Checksum[:], len(content))
	if err != nil {
		t.Fatalf("seed version: %v", err)
	}

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = p.db.Querier(bg).Exec(bg, `DELETE FROM users WHERE id = $1`, userID)
		_ = p.store.Delete(bg, storageKey)
	})
	return userID, documentID, versionID, storageKey
}

func (p *pipeline) publishUploaded(t *testing.T, userID, documentID, versionID uuid.UUID, storageKey string, eventID string) {
	t.Helper()
	payload := map[string]any{
		"cv_id":        documentID.String(),
		"version_id":   versionID.String(),
		"user_id":      userID.String(),
		"storage_key":  storageKey,
		"content_type": docxType,
	}
	meta := events.Meta{CorrelationID: "pipeline-test-" + versionID.String()[:8], UserID: userID.String()}

	if eventID == "" {
		if err := p.bus.PublishWithMeta(context.Background(), events.TypeCVUploaded, 1, meta, payload); err != nil {
			t.Fatalf("publish cv.uploaded: %v", err)
		}
		return
	}

	// A fixed event id so the same message can be published twice, which is what a
	// redelivery after a lost ack looks like to a consumer.
	env, err := events.NewEnvelope(events.TypeCVUploaded, 1, meta, payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	env.EventID = eventID
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	// No MsgID, so JetStream's own duplicate window does not mask the handler
	// idempotency this test is here to exercise.
	if _, err := p.bus.JetStream().Publish(context.Background(), env.Subject(), body); err != nil {
		t.Fatalf("publish raw: %v", err)
	}
}

// awaitParseStatus polls until the version reaches a terminal parse status.
func (p *pipeline) awaitParseStatus(t *testing.T, versionID uuid.UUID, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var status string
	for time.Now().Before(deadline) {
		err := p.db.Querier(context.Background()).QueryRow(context.Background(),
			`SELECT parse_status::text FROM cv_versions WHERE id = $1`, versionID).Scan(&status)
		if err != nil {
			t.Fatalf("read parse_status: %v", err)
		}
		if status == want {
			return status
		}
		time.Sleep(200 * time.Millisecond)
	}
	return status
}

func TestAnUploadedCVReachesParsedThroughTheRealPipeline(t *testing.T) {
	// Up from cv.uploaded through av-scan, text-extract and the structure stage, against
	// real NATS, Postgres and MinIO. The stages are chained by events, so this is the
	// only way to find out whether they actually hand over: each one in isolation can
	// pass while the chain never completes.
	p := startPipeline(t)

	content, err := os.ReadFile("../../platform/docextract/testdata/cv_simple.docx")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	userID, documentID, versionID, key := p.uploadCV(t, content)
	p.publishUploaded(t, userID, documentID, versionID, key, "")

	if status := p.awaitParseStatus(t, versionID, "parsed", 30*time.Second); status != "parsed" {
		t.Fatalf("parse_status = %q after 30s, want parsed; the chain did not complete", status)
	}

	// The derived state each stage was responsible for.
	var (
		avStatus  string
		rawText   *string
		charCount *int
	)
	err = p.db.Querier(context.Background()).QueryRow(context.Background(),
		`SELECT av_status::text, raw_text, char_count FROM cv_versions WHERE id = $1`,
		versionID).Scan(&avStatus, &rawText, &charCount)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}

	if avStatus != "clean" {
		t.Errorf("av_status = %q, want clean", avStatus)
	}
	if rawText == nil || *rawText == "" {
		t.Error("raw_text is empty; the text-extract stage produced nothing")
	}
	if charCount == nil || *charCount == 0 {
		t.Error("char_count is unset")
	}
}

func TestARedeliveredUploadDoesNotDuplicateDerivedRows(t *testing.T) {
	// JetStream is at-least-once, so an ack lost to a network blip or a worker dying
	// after its work but before its ack results in the same message arriving twice.
	// Republishing the identical envelope is exactly what the consumer sees.
	p := startPipeline(t)

	content, err := os.ReadFile("../../platform/docextract/testdata/cv_simple.docx")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	userID, documentID, versionID, key := p.uploadCV(t, content)
	eventID := "01TESTREDELIVERY" + versionID.String()[:8]

	p.publishUploaded(t, userID, documentID, versionID, key, eventID)
	if status := p.awaitParseStatus(t, versionID, "parsed", 30*time.Second); status != "parsed" {
		t.Fatalf("parse_status = %q, want parsed before testing redelivery", status)
	}

	before := countDerived(t, p.db, versionID)

	// The same event again.
	p.publishUploaded(t, userID, documentID, versionID, key, eventID)
	time.Sleep(5 * time.Second) // let it be delivered and dropped

	after := countDerived(t, p.db, versionID)

	if after.skills != before.skills {
		t.Errorf("cv_skills rows went from %d to %d on redelivery", before.skills, after.skills)
	}
	if after.parsed != before.parsed {
		t.Errorf("cv_parsed_data rows went from %d to %d on redelivery", before.parsed, after.parsed)
	}
	if after.versions != before.versions {
		t.Errorf("cv_versions rows for this document went from %d to %d on redelivery",
			before.versions, after.versions)
	}
}

type derivedCounts struct {
	skills   int
	parsed   int
	versions int
}

func countDerived(t *testing.T, db *postgres.DB, versionID uuid.UUID) derivedCounts {
	t.Helper()
	ctx := context.Background()
	var c derivedCounts
	err := db.Querier(ctx).QueryRow(ctx, `
		SELECT (SELECT count(*) FROM cv_skills WHERE cv_version_id = $1),
		       (SELECT count(*) FROM cv_parsed_data WHERE cv_version_id = $1),
		       (SELECT count(*) FROM cv_versions WHERE id = $1)`,
		versionID).Scan(&c.skills, &c.parsed, &c.versions)
	if err != nil {
		t.Fatalf("count derived rows: %v", err)
	}
	return c
}

func TestAnUndecodableMessageReachesTheDeadLetterQueue(t *testing.T) {
	// A poison message must end up somewhere an operator can find it rather than
	// cycling forever or vanishing. This is also what the dead-letter depth alert
	// watches, so the two have to agree about where such a message goes.
	p := startPipeline(t)
	ctx := context.Background()

	js := p.bus.JetStream()
	before := dlqDepth(t, ctx, js)

	// Structurally valid JSON that is not an envelope, published straight onto a cv
	// subject, as a producer with a stale schema would.
	if _, err := js.Publish(ctx, "myjobfyt.cv.uploaded", []byte(`{"not":"an envelope"}`)); err != nil {
		t.Fatalf("publish poison: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if dlqDepth(t, ctx, js) > before {
			return // it arrived
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("the dead-letter stream is still at %d after 30s; the poison message was "+
		"neither processed nor dead-lettered", before)
}

func dlqDepth(t *testing.T, ctx context.Context, js jetstream.JetStream) uint64 {
	t.Helper()
	stream, err := js.Stream(ctx, "DLQ")
	if err != nil {
		t.Fatalf("DLQ stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("DLQ info: %v", err)
	}
	return info.State.Msgs
}

func TestADeadLetterRecordsWhichStageGaveUp(t *testing.T) {
	// A dead letter with no provenance is nearly useless: the operator has an opaque
	// payload and no idea which consumer rejected it or why.
	p := startPipeline(t)
	ctx := context.Background()
	js := p.bus.JetStream()

	marker := uuid.New().String()
	if _, err := js.Publish(ctx, "myjobfyt.cv.uploaded",
		[]byte(fmt.Sprintf(`{"poison":%q}`, marker))); err != nil {
		t.Fatalf("publish poison: %v", err)
	}

	stream, err := js.Stream(ctx, "DLQ")
	if err != nil {
		t.Fatalf("DLQ stream: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// Routed by the rejecting consumer's domain: the message carried no type of
		// its own, but cv-av-scan knows which domain it serves.
		msg, err := stream.GetLastMsgForSubject(ctx, "myjobfyt.dlq.cv")
		if err == nil && bytes.Contains(msg.Data, []byte(marker)) {
			var record map[string]any
			if err := json.Unmarshal(msg.Data, &record); err != nil {
				t.Fatalf("the dead letter is not JSON: %v", err)
			}
			for _, field := range []string{"consumer", "reason"} {
				if v, ok := record[field]; !ok || v == "" {
					t.Errorf("the dead letter has no %q: %v", field, record)
				}
			}
			if record["consumer"] != "cv-av-scan" {
				t.Errorf("consumer = %v, want the stage that rejected it", record["consumer"])
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("no dead letter carrying this payload arrived within 30s")
}
