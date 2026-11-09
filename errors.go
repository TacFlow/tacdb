// Error API of tacdb (SPEC §6.1). Codes and Error live in internal/errs so
// that internal packages can return them without importing this package.

package tacdb

import "github.com/TacFlow/tacdb/internal/errs"

// Code is a stable error code. Codes never change once released.
type Code = errs.Code

// Error is the error type returned by tacdb. errors.Is matches by Code, so
// errors.Is(err, ErrCorrupt) reports whether err is a CORRUPT error.
type Error = errs.Error

// Location points into a query source.
type Location = errs.Location

// Stage is the stage of query processing an error comes from.
type Stage = errs.Stage

// Stages of query processing.
const (
	StageLex      = errs.StageLex
	StageParse    = errs.StageParse
	StageSemantic = errs.StageSemantic
	StagePlan     = errs.StagePlan
	StageExecute  = errs.StageExecute
)

// Lifecycle codes.
const (
	CodeClosed              = errs.CodeClosed
	CodeBusy                = errs.CodeBusy
	CodeTxClosed            = errs.CodeTxClosed
	CodeTxInUse             = errs.CodeTxInUse
	CodeReadOnly            = errs.CodeReadOnly
	CodeDBBusy              = errs.CodeDBBusy
	CodeBusyTimeout         = errs.CodeBusyTimeout
	CodeWriterQueueFull     = errs.CodeWriterQueueFull
	CodeTooManyTransactions = errs.CodeTooManyTransactions
	CodeSnapshotExpired     = errs.CodeSnapshotExpired
	CodeWriteTxTimeout      = errs.CodeWriteTxTimeout
	CodeShuttingDown        = errs.CodeShuttingDown
)

// Input codes.
const (
	CodeInvalidArgument = errs.CodeInvalidArgument
	CodeTypeMismatch    = errs.CodeTypeMismatch
	CodeLimitExceeded   = errs.CodeLimitExceeded
	CodeNotFound        = errs.CodeNotFound
	CodeDuplicateKey    = errs.CodeDuplicateKey
)

// Query codes.
const (
	CodeQueryLex               = errs.CodeQueryLex
	CodeQueryParse             = errs.CodeQueryParse
	CodeQuerySemantic          = errs.CodeQuerySemantic
	CodeQueryUnsupported       = errs.CodeQueryUnsupported
	CodeQueryWriteRequiresExec = errs.CodeQueryWriteRequiresExec
	CodeParamMissing           = errs.CodeParamMissing
	CodeParamType              = errs.CodeParamType
)

// Persistence codes.
const (
	CodeLocked            = errs.CodeLocked
	CodeIO                = errs.CodeIO
	CodeNoSpace           = errs.CodeNoSpace
	CodeCorrupt           = errs.CodeCorrupt
	CodeUnsupportedFormat = errs.CodeUnsupportedFormat
	CodeWALLimit          = errs.CodeWALLimit
	CodeCommitUnknown     = errs.CodeCommitUnknown
	CodeCommitPending     = errs.CodeCommitPending
	CodeCommitQueueFull   = errs.CodeCommitQueueFull
	CodeRecoveryRequired  = errs.CodeRecoveryRequired
)

// Backup codes.
const (
	CodeBackupLag         = errs.CodeBackupLag
	CodeBackupInvalid     = errs.CodeBackupInvalid
	CodeDestinationExists = errs.CodeDestinationExists
)

// Indexes codes.
const (
	CodeIndexNotFound        = errs.CodeIndexNotFound
	CodeIndexExistsDifferent = errs.CodeIndexExistsDifferent
	CodeInvalidVector        = errs.CodeInvalidVector
	CodeVectorDisabled       = errs.CodeVectorDisabled
	CodeVectorKeyMismatch    = errs.CodeVectorKeyMismatch
	CodeIndexInvalid         = errs.CodeIndexInvalid
	CodeIndexInconsistent    = errs.CodeIndexInconsistent
	CodeIndexBuilding        = errs.CodeIndexBuilding
	CodeIndexBuildFailed     = errs.CodeIndexBuildFailed
	CodeUniqueViolation      = errs.CodeUniqueViolation
)

// Events codes.
const (
	CodeReservedStream   = errs.CodeReservedStream
	CodeCursorExpired    = errs.CodeCursorExpired
	CodeOffsetRegression = errs.CodeOffsetRegression
	CodeInvalidSequence  = errs.CodeInvalidSequence
)

// Execution codes.
const (
	CodeCanceled        = errs.CodeCanceled
	CodeDeadline        = errs.CodeDeadline
	CodeResourceLimit   = errs.CodeResourceLimit
	CodeQueryIncomplete = errs.CodeQueryIncomplete
)

// Cluster and IPC codes.
const (
	CodeNotLeader             = errs.CodeNotLeader
	CodeNoQuorum              = errs.CodeNoQuorum
	CodeLeaderChanged         = errs.CodeLeaderChanged
	CodeReplicaLag            = errs.CodeReplicaLag
	CodeSessionExpired        = errs.CodeSessionExpired
	CodeCursorLost            = errs.CodeCursorLost
	CodeClusterModeRequired   = errs.CodeClusterModeRequired
	CodeVersionIncompatible   = errs.CodeVersionIncompatible
	CodeInvalidDurabilityMode = errs.CodeInvalidDurabilityMode
	CodeRequestConflict       = errs.CodeRequestConflict
	CodeRequestOutOfOrder     = errs.CodeRequestOutOfOrder
	CodeResultExpired         = errs.CodeResultExpired
	CodeRequestAlreadyApplied = errs.CodeRequestAlreadyApplied
)

// Security codes.
const (
	CodeUnauthenticated     = errs.CodeUnauthenticated
	CodePermissionDenied    = errs.CodePermissionDenied
	CodePolicyViolation     = errs.CodePolicyViolation
	CodeClassifiedValue     = errs.CodeClassifiedValue
	CodeQuotaExceeded       = errs.CodeQuotaExceeded
	CodeSignatureRequired   = errs.CodeSignatureRequired
	CodeSignatureInvalid    = errs.CodeSignatureInvalid
	CodeUnsafePath          = errs.CodeUnsafePath
	CodeInsecurePermissions = errs.CodeInsecurePermissions
)

// Sentinels for lifecycle codes, for use with errors.Is.
var (
	ErrClosed              = errs.Sentinel(CodeClosed)
	ErrBusy                = errs.Sentinel(CodeBusy)
	ErrTxClosed            = errs.Sentinel(CodeTxClosed)
	ErrTxInUse             = errs.Sentinel(CodeTxInUse)
	ErrReadOnly            = errs.Sentinel(CodeReadOnly)
	ErrDBBusy              = errs.Sentinel(CodeDBBusy)
	ErrBusyTimeout         = errs.Sentinel(CodeBusyTimeout)
	ErrWriterQueueFull     = errs.Sentinel(CodeWriterQueueFull)
	ErrTooManyTransactions = errs.Sentinel(CodeTooManyTransactions)
	ErrSnapshotExpired     = errs.Sentinel(CodeSnapshotExpired)
	ErrWriteTxTimeout      = errs.Sentinel(CodeWriteTxTimeout)
	ErrShuttingDown        = errs.Sentinel(CodeShuttingDown)
)

// Sentinels for input codes, for use with errors.Is.
var (
	ErrInvalidArgument = errs.Sentinel(CodeInvalidArgument)
	ErrTypeMismatch    = errs.Sentinel(CodeTypeMismatch)
	ErrLimitExceeded   = errs.Sentinel(CodeLimitExceeded)
	ErrNotFound        = errs.Sentinel(CodeNotFound)
	ErrDuplicateKey    = errs.Sentinel(CodeDuplicateKey)
)

// Sentinels for query codes, for use with errors.Is.
var (
	ErrQueryLex               = errs.Sentinel(CodeQueryLex)
	ErrQueryParse             = errs.Sentinel(CodeQueryParse)
	ErrQuerySemantic          = errs.Sentinel(CodeQuerySemantic)
	ErrQueryUnsupported       = errs.Sentinel(CodeQueryUnsupported)
	ErrQueryWriteRequiresExec = errs.Sentinel(CodeQueryWriteRequiresExec)
	ErrParamMissing           = errs.Sentinel(CodeParamMissing)
	ErrParamType              = errs.Sentinel(CodeParamType)
)

// Sentinels for persistence codes, for use with errors.Is.
var (
	ErrLocked            = errs.Sentinel(CodeLocked)
	ErrIO                = errs.Sentinel(CodeIO)
	ErrNoSpace           = errs.Sentinel(CodeNoSpace)
	ErrCorrupt           = errs.Sentinel(CodeCorrupt)
	ErrUnsupportedFormat = errs.Sentinel(CodeUnsupportedFormat)
	ErrWALLimit          = errs.Sentinel(CodeWALLimit)
	ErrCommitUnknown     = errs.Sentinel(CodeCommitUnknown)
	ErrCommitPending     = errs.Sentinel(CodeCommitPending)
	ErrCommitQueueFull   = errs.Sentinel(CodeCommitQueueFull)
	ErrRecoveryRequired  = errs.Sentinel(CodeRecoveryRequired)
)

// Sentinels for backup codes, for use with errors.Is.
var (
	ErrBackupLag         = errs.Sentinel(CodeBackupLag)
	ErrBackupInvalid     = errs.Sentinel(CodeBackupInvalid)
	ErrDestinationExists = errs.Sentinel(CodeDestinationExists)
)

// Sentinels for indexes codes, for use with errors.Is.
var (
	ErrIndexNotFound        = errs.Sentinel(CodeIndexNotFound)
	ErrIndexExistsDifferent = errs.Sentinel(CodeIndexExistsDifferent)
	ErrInvalidVector        = errs.Sentinel(CodeInvalidVector)
	ErrVectorDisabled       = errs.Sentinel(CodeVectorDisabled)
	ErrVectorKeyMismatch    = errs.Sentinel(CodeVectorKeyMismatch)
	ErrIndexInvalid         = errs.Sentinel(CodeIndexInvalid)
	ErrIndexInconsistent    = errs.Sentinel(CodeIndexInconsistent)
	ErrIndexBuilding        = errs.Sentinel(CodeIndexBuilding)
	ErrIndexBuildFailed     = errs.Sentinel(CodeIndexBuildFailed)
	ErrUniqueViolation      = errs.Sentinel(CodeUniqueViolation)
)

// Sentinels for events codes, for use with errors.Is.
var (
	ErrReservedStream   = errs.Sentinel(CodeReservedStream)
	ErrCursorExpired    = errs.Sentinel(CodeCursorExpired)
	ErrOffsetRegression = errs.Sentinel(CodeOffsetRegression)
	ErrInvalidSequence  = errs.Sentinel(CodeInvalidSequence)
)

// Sentinels for execution codes, for use with errors.Is.
var (
	ErrCanceled        = errs.Sentinel(CodeCanceled)
	ErrDeadline        = errs.Sentinel(CodeDeadline)
	ErrResourceLimit   = errs.Sentinel(CodeResourceLimit)
	ErrQueryIncomplete = errs.Sentinel(CodeQueryIncomplete)
)

// Sentinels for cluster and ipc codes, for use with errors.Is.
var (
	ErrNotLeader             = errs.Sentinel(CodeNotLeader)
	ErrNoQuorum              = errs.Sentinel(CodeNoQuorum)
	ErrLeaderChanged         = errs.Sentinel(CodeLeaderChanged)
	ErrReplicaLag            = errs.Sentinel(CodeReplicaLag)
	ErrSessionExpired        = errs.Sentinel(CodeSessionExpired)
	ErrCursorLost            = errs.Sentinel(CodeCursorLost)
	ErrClusterModeRequired   = errs.Sentinel(CodeClusterModeRequired)
	ErrVersionIncompatible   = errs.Sentinel(CodeVersionIncompatible)
	ErrInvalidDurabilityMode = errs.Sentinel(CodeInvalidDurabilityMode)
	ErrRequestConflict       = errs.Sentinel(CodeRequestConflict)
	ErrRequestOutOfOrder     = errs.Sentinel(CodeRequestOutOfOrder)
	ErrResultExpired         = errs.Sentinel(CodeResultExpired)
	ErrRequestAlreadyApplied = errs.Sentinel(CodeRequestAlreadyApplied)
)

// Sentinels for security codes, for use with errors.Is.
var (
	ErrUnauthenticated     = errs.Sentinel(CodeUnauthenticated)
	ErrPermissionDenied    = errs.Sentinel(CodePermissionDenied)
	ErrPolicyViolation     = errs.Sentinel(CodePolicyViolation)
	ErrClassifiedValue     = errs.Sentinel(CodeClassifiedValue)
	ErrQuotaExceeded       = errs.Sentinel(CodeQuotaExceeded)
	ErrSignatureRequired   = errs.Sentinel(CodeSignatureRequired)
	ErrSignatureInvalid    = errs.Sentinel(CodeSignatureInvalid)
	ErrUnsafePath          = errs.Sentinel(CodeUnsafePath)
	ErrInsecurePermissions = errs.Sentinel(CodeInsecurePermissions)
)
