package events

import (
	"fmt"
	"strings"
)

// subjectPrefix namespaces every subject, so a shared NATS cluster can host other
// workloads without collision.
const subjectPrefix = "myjobfyt"

// Event type constants. These strings are permanent: a renamed concept gets a new type
// plus a transitional consumer, never a redefinition, because a stream may still hold
// messages published under the old name.
const (
	// identity
	TypeCandidateCreated         = "identity.candidate.created"
	TypeEmailVerified            = "identity.email.verified"
	TypeProfileUpdated           = "identity.profile.updated"
	TypeAccountDeletionRequested = "identity.account.deletion_requested"
	TypeAccountErased            = "identity.account.erased"

	// cv
	TypeCVUploaded           = "cv.uploaded"
	TypeCVScanned            = "cv.scanned"
	TypeCVTextExtracted      = "cv.text_extracted"
	TypeCVParsed             = "cv.parsed"
	TypeCVParseFailed        = "cv.parse_failed"
	TypeCVAnalysisRequested  = "cv.analysis_requested"
	TypeCVAnalysisCompleted  = "cv.analysis_completed"
	TypeCVTailoringRequested = "cv.tailoring_requested"
	TypeCVTailored           = "cv.tailored"

	// jobs
	TypeDiscoveryRequested = "jobs.discovery.requested"
	TypeJobDiscovered      = "jobs.discovered"
	TypeJobNormalised      = "jobs.normalised"
	TypeJobUpdated         = "jobs.updated"
	TypeJobExpired         = "jobs.expired"
	TypeSourceDegraded     = "jobs.source.degraded"

	// matching
	TypeMatchingRequested = "matching.requested"
	TypeJobMatched        = "matching.matched"
	TypeJobRejected       = "matching.rejected"
	TypeBackfillRequested = "matching.backfill_requested"

	// applications
	TypeApplicationPreparationRequested = "applications.preparation_requested"
	TypeApplicationMethodSelected       = "applications.method_selected"
	TypeApplicationPrepared             = "applications.prepared"
	TypeApplicationQuestionsPending     = "applications.questions_pending"
	TypeApplicationQuestionsAnswered    = "applications.questions_answered"
	TypeApplicationAuthorised           = "applications.authorised"
	TypeApplicationSubmitted            = "applications.submitted"
	TypeApplicationFailed               = "applications.failed"
	TypeApplicationOutcomeUnknown       = "applications.outcome_unknown"
	TypeApplicationStatusUpdated        = "applications.status_updated"

	// outreach
	TypeOutreachSendRequested = "outreach.send_requested"
	TypeOutreachSent          = "outreach.sent"
	TypeOutreachOpened        = "outreach.opened"
	TypeOutreachReplied       = "outreach.replied"
	TypeOutreachBounced       = "outreach.bounced"
	TypeOutreachComplained    = "outreach.complained"

	// billing
	TypeSubscriptionActivated = "billing.subscription.activated"
	TypeSubscriptionUpdated   = "billing.subscription.updated"
	TypeSubscriptionCanceled  = "billing.subscription.canceled"
	TypePaymentFailed         = "billing.payment.failed"
	TypeQuotaExhausted        = "billing.quota.exhausted"
)

// SubjectFor maps an event type to its fully-qualified subject.
func SubjectFor(eventType string) string {
	return subjectPrefix + "." + eventType
}

// DomainOf extracts the domain (first segment) of an event type.
func DomainOf(eventType string) string {
	if i := strings.IndexByte(eventType, '.'); i > 0 {
		return eventType[:i]
	}
	return eventType
}

// DeadLetterSubject returns the dead-letter subject for an event type's domain.
func DeadLetterSubject(eventType string) string {
	return fmt.Sprintf("%s.dlq.%s", subjectPrefix, DomainOf(eventType))
}
