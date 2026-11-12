package cv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/redisx"
	"github.com/google/uuid"
)

// CheckCache stores completed reports, keyed by CV version and target role.
//
// A re-check of an unchanged CV against the same role costs nothing, which matters
// because this is a free-tier feature and candidates refresh it while editing.
type CheckCache interface {
	Get(ctx context.Context, key string) (CheckReport, bool)
	Put(ctx context.Context, key string, report CheckReport) error
}

// CheckService produces a CV's assessment.
type CheckService struct {
	repo     Repository
	analyser CheckAnalyser
	cache    CheckCache
	log      *slog.Logger
}

// NewCheckService builds the service.
//
// analyser may be nil, which is a supported mode rather than a failure: the
// deterministic rubric still produces a score, and the report says which categories
// were not assessed.
func NewCheckService(repo Repository, analyser CheckAnalyser, cache CheckCache, log *slog.Logger) *CheckService {
	return &CheckService{repo: repo, analyser: analyser, cache: cache, log: log}
}

// Check assesses a CV version, optionally against a target role.
//
// The deterministic pass always runs. The model pass is attempted only when an analyser
// is configured and a target role was given, and a failure there degrades the report
// rather than failing the request: a partial assessment that says what is missing is
// far more use to a candidate than an error.
func (s *CheckService) Check(ctx context.Context, userID, versionID uuid.UUID, targetRole string) (CheckReport, error) {
	version, err := s.repo.VersionByID(ctx, userID, versionID)
	if err != nil {
		return CheckReport{}, err
	}

	log := s.log.With(
		slog.String("cv_version_id", versionID.String()),
		slog.String("user_id", userID.String()),
	)

	cacheKey := checkCacheKey(versionID, targetRole, version.ParsedAt)
	if s.cache != nil {
		if cached, ok := s.cache.Get(ctx, cacheKey); ok {
			log.InfoContext(ctx, "cv check served from cache")
			return cached, nil
		}
	}

	parsed, err := s.repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
		return CheckReport{}, err
	}

	text := ExtractedText{}
	if version.CharCount != nil {
		text.CharCount = *version.CharCount
	}
	if version.PageCount != nil {
		text.PageCount = *version.PageCount
	}
	rawText, err := s.repo.RawTextForVersion(ctx, userID, versionID)
	if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
		return CheckReport{}, err
	}
	text.Text = rawText

	report := CheckCV(parsed, text)

	// The model pass is additive. Everything above already stands on its own.
	if s.analyser != nil && targetRole != "" && text.HasUsableText() {
		analysis, aerr := s.analyser.AnalyseCV(ctx, CheckAnalysisInput{
			CVText: text.Text, TargetRole: targetRole,
		})
		switch {
		case aerr != nil:
			// Degrade honestly. The report already declares keyword coverage missing,
			// so the candidate is told what they are not getting rather than shown a
			// number that quietly means less than it appears to.
			log.WarnContext(ctx, "cv check falling back to the deterministic rubric",
				slog.Any("error", aerr))
		default:
			if analysis.InjectionAttempt != "" {
				log.WarnContext(ctx, "the target role contained text addressed to the model",
					slog.Int("injection_text_length", len(analysis.InjectionAttempt)))
			}
			report = ApplyAnalysis(report, analysis)
		}
	}

	if s.cache != nil {
		if perr := s.cache.Put(ctx, cacheKey, report); perr != nil {
			// Not fatal: a cache miss next time costs a recomputation, nothing more.
			log.WarnContext(ctx, "could not cache a cv check", slog.Any("error", perr))
		}
	}

	log.InfoContext(ctx, "cv checked",
		slog.Int("overall", report.Overall),
		slog.Int("coverage", report.Coverage),
		slog.Bool("degraded", report.Degraded),
		slog.Int("findings", len(report.Findings)),
	)
	return report, nil
}

// checkCacheKey identifies a report by what produced it.
//
// Includes the rubric version, so a rubric change invalidates every cached report
// rather than serving scores computed by rules that no longer exist; and the parse
// timestamp, so a re-parse of the same version id is a different CV for caching
// purposes.
func checkCacheKey(versionID uuid.UUID, targetRole string, parsedAt *time.Time) string {
	h := sha256.New()
	h.Write([]byte(versionID.String()))
	h.Write([]byte{0})
	h.Write([]byte(targetRole))
	h.Write([]byte{0})
	h.Write([]byte(rubricVersion))
	h.Write([]byte{0})
	if parsedAt != nil {
		h.Write([]byte(parsedAt.UTC().Format(time.RFC3339Nano)))
	}
	return "cv:check:" + hex.EncodeToString(h.Sum(nil))
}

// RedisCheckCache caches reports in Redis.
type RedisCheckCache struct {
	redis *redisx.Client
	ttl   time.Duration
}

// checkCacheTTL bounds a cached report. Short enough that a rubric or prompt change
// takes effect within a day even for a CV nobody touches.
const checkCacheTTL = 24 * time.Hour

// NewRedisCheckCache builds the cache.
func NewRedisCheckCache(redis *redisx.Client) *RedisCheckCache {
	return &RedisCheckCache{redis: redis, ttl: checkCacheTTL}
}

func (c *RedisCheckCache) Get(ctx context.Context, key string) (CheckReport, bool) {
	body, err := c.redis.Raw().Get(ctx, key).Bytes()
	if err != nil {
		return CheckReport{}, false
	}
	var report CheckReport
	if err := json.Unmarshal(body, &report); err != nil {
		// A corrupt entry is a cache miss, not an error: recomputing is cheap and
		// correct, and failing the request over a cache would be absurd.
		return CheckReport{}, false
	}
	return report, true
}

func (c *RedisCheckCache) Put(ctx context.Context, key string, report CheckReport) error {
	body, err := json.Marshal(report)
	if err != nil {
		return apperr.Internalf(err, "marshal cv check report")
	}
	if err := c.redis.Raw().Set(ctx, key, body, c.ttl).Err(); err != nil {
		return apperr.Internalf(err, "cache cv check report")
	}
	return nil
}
