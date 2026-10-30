import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// Mirrors checkpoint fields and privacy allowlists in server/collector.go,
// server/import.go, server/usage_history.go, server/tokscale_groups.go, and server/usage_privacy_test.go.
const sqlite = process.getBuiltinModule("node:sqlite");
if (!sqlite) throw new Error("Session Cost state inspection requires the Node.js SQLite builtin");
const { DatabaseSync } = sqlite;

export const SESSION_COST_PLUGIN_ID = "kandev-session-cost";
export const SESSION_COST_DEFAULT_DATABASE = "kandev.db";
export const SESSION_COST_AUTH_DATABASE = "kandev-auth-usage-history.db";

const allowedDatabaseNames = new Set([
  SESSION_COST_DEFAULT_DATABASE,
  SESSION_COST_AUTH_DATABASE,
]);
const hashPattern = /^sha256:[0-9a-f]{64}$/;
const opaquePattern = /^[A-Za-z0-9][A-Za-z0-9._:+/@=-]{0,511}$/;
const datePattern = /^\d{4}-\d{2}-\d{2}$/;
const timestampPattern = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const safeImportStatuses = new Set([
  "not_started",
  "running",
  "preparing",
  "completed",
  "failed",
  "cancelled",
  "disabled",
  "unsupported",
]);
const safeSessionStates = new Set([
  "CREATED",
  "STARTING",
  "RUNNING",
  "IDLE",
  "WAITING_FOR_INPUT",
  "COMPLETED",
  "FAILED",
  "CANCELLED",
]);
const forbiddenFieldParts = [
  "token",
  "cost",
  "amount",
  "price",
  "prompt",
  "title",
  "transcript",
  "message",
  "model",
  "provider",
  "email",
  "displayname",
  "person",
  "private",
  "personaldata",
  "privatedata",
  "rawsourcelabel",
];

const schemaNames = [
  "collector-checkpoint",
  "historical-import-checkpoint",
  "privacy-migration-checkpoint",
  "usage-observation-receipt",
  "usage-target-checkpoint",
  "tokscale-membership-checkpoint",
];

function fail(reason) {
  throw new Error(`Retained usage plugin-state privacy inspection failed: ${reason}`);
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function assertObject(value, allowedFields, requiredFields = []) {
  if (!isObject(value)) fail("checkpoint shape is not an object");
  const allowed = new Set(allowedFields);
  for (const key of Object.keys(value)) {
    const normalized = key.replace(/[^a-z0-9]/gi, "").toLowerCase();
    if (forbiddenFieldParts.some((part) => normalized.includes(part))) {
      fail("checkpoint contains a prohibited accounting or personal-data field");
    }
    if (!allowed.has(key)) fail("checkpoint contains an unapproved field");
  }
  if (requiredFields.some((key) => !Object.hasOwn(value, key))) {
    fail("checkpoint is missing a required field");
  }
  return value;
}

function assertString(value, { empty = false, pattern = opaquePattern } = {}) {
  if (typeof value !== "string" || (!empty && value.length === 0) || (value && !pattern.test(value))) {
    fail("checkpoint contains an invalid string value");
  }
}

function assertOptionalString(object, key, options) {
  if (Object.hasOwn(object, key)) assertString(object[key], options);
}

function assertOptionalNullableString(object, key, options) {
  if (Object.hasOwn(object, key) && object[key] !== null) assertString(object[key], options);
}

function assertBoolean(value) {
  if (typeof value !== "boolean") fail("checkpoint contains an invalid boolean value");
}

function assertNonnegativeInteger(value) {
  if (!Number.isSafeInteger(value) || value < 0) fail("checkpoint contains an invalid scheduling count");
}

function assertDate(value) {
  assertString(value, { pattern: datePattern });
  if (Number.isNaN(Date.parse(`${value}T00:00:00Z`))) fail("checkpoint contains an invalid date");
}

function assertTimestamp(value) {
  assertString(value, { pattern: timestampPattern });
  if (Number.isNaN(Date.parse(value))) fail("checkpoint contains an invalid timestamp");
}

function assertHash(value) {
  assertString(value, { pattern: hashPattern });
}

function assertDateArray(value) {
  if (!Array.isArray(value)) fail("checkpoint contains an invalid date list");
  for (const date of value) assertDate(date);
}

function assertOpaqueArray(value, maxLength = 10_000) {
  if (!Array.isArray(value) || value.length > maxLength) fail("checkpoint contains an invalid identity list");
  for (const identity of value) assertString(identity);
}

function assertIdentityMap(value, validateEntry) {
  if (!isObject(value)) fail("checkpoint contains an invalid identity map");
  for (const [identity, entry] of Object.entries(value)) {
    assertString(identity);
    validateEntry(entry);
  }
}

function scanPrivateMarkers(value, markers) {
  if (typeof value === "string") {
    const lowerValue = value.toLowerCase();
    if (markers.some((marker) => {
      const lowerMarker = marker.toLowerCase();
      return lowerValue === lowerMarker || (marker.length >= 8 && lowerValue.includes(lowerMarker));
    })) {
      fail("checkpoint contains a supplied private fixture marker");
    }
    return;
  }
  if (Array.isArray(value)) {
    for (const entry of value) scanPrivateMarkers(entry, markers);
    return;
  }
  if (isObject(value)) {
    for (const [key, entry] of Object.entries(value)) {
      scanPrivateMarkers(key, markers);
      scanPrivateMarkers(entry, markers);
    }
  }
}

function assertUsageTarget(value) {
  const target = assertObject(value, [
    "SourceScopeStatus",
    "SessionStartedAt",
    "SessionEndedAt",
    "CoverageStart",
    "CoverageEnd",
    "SubjectBinding",
    "TaskID",
    "SessionID",
    "ExternalSessionID",
    "SubjectKind",
    "ActorAttributionStatus",
    "SnapshotID",
    "SnapshotUpperBound",
    "Quarantined",
  ]);
  if (Object.hasOwn(target, "SourceScopeStatus") && !["eligible", "mixed", "unknown"].includes(target.SourceScopeStatus)) {
    fail("checkpoint contains an invalid source status");
  }
  for (const key of ["SessionStartedAt", "SessionEndedAt", "CoverageStart", "CoverageEnd"]) {
    assertOptionalNullableString(target, key, { pattern: timestampPattern });
  }
  for (const key of ["SubjectBinding", "TaskID", "SubjectKind", "ActorAttributionStatus", "SnapshotID"]) {
    assertOptionalString(target, key, { empty: key === "SubjectKind" || key === "ActorAttributionStatus" });
  }
  for (const key of ["SessionID", "ExternalSessionID"]) assertOptionalNullableString(target, key);
  if (Object.hasOwn(target, "SnapshotUpperBound")) assertNonnegativeInteger(target.SnapshotUpperBound);
  if (Object.hasOwn(target, "Quarantined")) assertBoolean(target.Quarantined);
}

function parseWrappedJSON(value) {
  const wrapper = assertObject(value, ["json"], ["json"]);
  if (typeof wrapper.json !== "string") fail("checkpoint JSON envelope is invalid");
  try {
    return JSON.parse(wrapper.json);
  } catch {
    fail("checkpoint JSON is malformed");
  }
}

function validatePrivacyMigration(value) {
  const state = assertObject(value, [
    "Version",
    "Complete",
    "Namespace",
    "Cursor",
    "PageLoaded",
    "PendingKeys",
    "PageNextCursor",
  ], ["Version", "Complete"]);
  if (state.Version !== 2) fail("privacy migration checkpoint version is unsupported");
  assertBoolean(state.Complete);
  if (Object.hasOwn(state, "Namespace")) {
    assertNonnegativeInteger(state.Namespace);
    if (state.Namespace > 2) fail("privacy migration checkpoint namespace is invalid");
  }
  assertOptionalString(state, "Cursor", { empty: true });
  if (Object.hasOwn(state, "PageLoaded")) assertBoolean(state.PageLoaded);
  if (Object.hasOwn(state, "PendingKeys")) assertOpaqueArray(state.PendingKeys, 200);
  assertOptionalString(state, "PageNextCursor", { empty: true });
}

function validateUsageReceipt(value, row) {
  const receipt = assertObject(value, [
    "Version",
    "ContentDigest",
    "SubjectBinding",
    "ItemID",
    "Revision",
    "PayloadDigest",
    "Accepted",
  ], ["Version", "ContentDigest", "SubjectBinding", "ItemID", "Revision", "PayloadDigest", "Accepted"]);
  if (receipt.Version !== 2) fail("usage receipt version is unsupported");
  assertHash(receipt.ContentDigest);
  assertString(receipt.SubjectBinding);
  assertHash(receipt.ItemID);
  assertNonnegativeInteger(receipt.Revision);
  assertHash(receipt.PayloadDigest);
  assertBoolean(receipt.Accepted);
  assertHash(row.state_key);
}

function validateMembership(value, row) {
  const state = assertObject(value, ["Version", "Members", "Quarantined"], ["Version"]);
  if (state.Version !== 1) fail("membership checkpoint version is unsupported");
  if (Object.hasOwn(state, "Members")) {
    if (!Array.isArray(state.Members) || state.Members.length > 1024) fail("membership checkpoint exceeds its bound");
    for (const member of state.Members) assertHash(member);
  }
  if (Object.hasOwn(state, "Quarantined")) assertBoolean(state.Quarantined);
  assertHash(row.state_key);
}

function validateUsageTarget(value, row) {
  assertUsageTarget(value);
  assertHash(row.state_key);
}

function validateCollectorPending(value) {
  const pending = assertObject(value, [
    "active_date",
    "rollover_dates",
    "subject_binding",
    "task_id",
    "workspace_id",
    "acp_session_id",
    "state",
    "updated_at",
    "attempts",
    "dates",
    "date_attempts",
  ], ["task_id", "workspace_id", "acp_session_id", "state"]);
  for (const key of ["active_date"]) if (Object.hasOwn(pending, key)) assertDate(pending[key]);
  for (const key of ["rollover_dates", "dates"]) if (Object.hasOwn(pending, key)) assertDateArray(pending[key]);
  for (const key of ["subject_binding", "task_id", "workspace_id", "acp_session_id"]) {
    assertOptionalString(pending, key);
  }
  if (!safeSessionStates.has(pending.state)) fail("collector checkpoint contains an invalid session state");
  if (Object.hasOwn(pending, "updated_at")) assertTimestamp(pending.updated_at);
  if (Object.hasOwn(pending, "attempts")) assertNonnegativeInteger(pending.attempts);
  if (Object.hasOwn(pending, "date_attempts")) {
    assertIdentityMap(pending.date_attempts, (attempts) => assertNonnegativeInteger(attempts));
    for (const date of Object.keys(pending.date_attempts)) assertDate(date);
  }
}

function validateCollector(value) {
  const checkpoint = assertObject(value, ["last_date", "last_scan_at", "initialized", "pending"], ["initialized"]);
  if (Object.hasOwn(checkpoint, "last_date")) assertDate(checkpoint.last_date);
  if (Object.hasOwn(checkpoint, "last_scan_at")) assertTimestamp(checkpoint.last_scan_at);
  assertBoolean(checkpoint.initialized);
  if (Object.hasOwn(checkpoint, "pending")) {
    assertIdentityMap(checkpoint.pending, validateCollectorPending);
  }
}

function validateImportedTarget(value) {
  const target = assertObject(value, [
    "SourceScopeStatus",
    "SessionStartedAt",
    "SessionEndedAt",
    "CoverageStart",
    "CoverageEnd",
    "SubjectBinding",
    "TaskID",
    "SessionID",
    "ExternalSessionID",
    "SubjectKind",
    "ActorAttributionStatus",
  ]);
  if (Object.hasOwn(target, "SourceScopeStatus") && !["eligible", "mixed", "unknown"].includes(target.SourceScopeStatus)) {
    fail("import checkpoint contains an invalid source status");
  }
  for (const key of ["SessionStartedAt", "SessionEndedAt", "CoverageStart", "CoverageEnd"]) {
    assertOptionalNullableString(target, key, { pattern: timestampPattern });
  }
  for (const key of ["SubjectBinding", "TaskID", "SubjectKind", "ActorAttributionStatus"]) {
    assertOptionalString(target, key, { empty: key === "SubjectKind" || key === "ActorAttributionStatus" });
  }
  for (const key of ["SessionID", "ExternalSessionID"]) assertOptionalNullableString(target, key);
}

function validateImport(value) {
  const state = assertObject(value, [
    "run_id",
    "schema_version",
    "diagnostics_reliable",
    "legacy_diagnostics",
    "snapshot_id",
    "snapshot_upper_bound",
    "page_targets",
    "page_next_cursor",
    "page_has_more",
    "page_dates",
    "page_found",
    "page_missing_ids",
    "workspace_id",
    "status",
    "cursor",
    "processed",
    "missing",
    "missing_session_ids",
    "last_error",
    "started_at",
    "finished_at",
    "last_successful_at",
    "undated",
    "pending_dates",
  ], ["schema_version", "diagnostics_reliable", "workspace_id", "status"]);
  if (state.schema_version !== 2) fail("historical import checkpoint version is unsupported");
  for (const key of ["diagnostics_reliable", "legacy_diagnostics", "page_has_more", "undated"]) {
    if (Object.hasOwn(state, key)) assertBoolean(state[key]);
  }
  for (const key of ["run_id", "snapshot_id", "page_next_cursor", "cursor", "workspace_id"]) {
    assertOptionalString(state, key, { empty: key === "page_next_cursor" || key === "cursor" });
  }
  if (Object.hasOwn(state, "snapshot_upper_bound")) {
    if (typeof state.snapshot_upper_bound !== "string" || !/^\d+$/.test(state.snapshot_upper_bound)) {
      fail("historical import checkpoint snapshot bound is invalid");
    }
  }
  if (!safeImportStatuses.has(state.status)) fail("historical import checkpoint status is invalid");
  for (const key of ["processed", "missing"]) if (Object.hasOwn(state, key)) assertNonnegativeInteger(state[key]);
  if (Object.hasOwn(state, "page_targets")) {
    if (!Array.isArray(state.page_targets) || state.page_targets.length > 50) fail("historical import target page exceeds its bound");
    for (const target of state.page_targets) validateImportedTarget(target);
  }
  if (Object.hasOwn(state, "page_dates")) {
    assertIdentityMap(state.page_dates, assertDateArray);
  }
  if (Object.hasOwn(state, "page_found")) {
    assertIdentityMap(state.page_found, assertBoolean);
  }
  for (const key of ["page_missing_ids", "missing_session_ids"]) {
    if (Object.hasOwn(state, key)) assertOpaqueArray(state[key], 100);
  }
  if (Object.hasOwn(state, "last_error") && state.last_error !== "") {
    fail("checkpoint contains free-form error text");
  }
  for (const key of ["started_at", "finished_at", "last_successful_at"]) {
    if (Object.hasOwn(state, key)) assertTimestamp(state[key]);
  }
  if (Object.hasOwn(state, "pending_dates")) assertDateArray(state.pending_dates);
}

function schemaForRow(row) {
  if (row.scope === "plugin" && row.scope_id === "canonical-usage-v1") {
    fail("legacy canonical accounting state remains in plugin storage");
  }
  if (row.scope === "plugin" && row.scope_id === "usage-state-privacy-v2" && row.state_key === "completed") {
    return "privacy-migration-checkpoint";
  }
  if (row.scope === "plugin" && row.scope_id === "usage-observation-v1" && hashPattern.test(row.state_key)) {
    return "usage-observation-receipt";
  }
  if (row.scope === "plugin" && row.scope_id === "usage-target-v1" && hashPattern.test(row.state_key)) {
    return "usage-target-checkpoint";
  }
  if (row.scope === "plugin" && row.scope_id === "tokscale-membership-v1" && hashPattern.test(row.state_key)) {
    return "tokscale-membership-checkpoint";
  }
  if (row.scope === "plugin" && row.scope_id === "collector" && row.state_key === "checkpoint") {
    return "collector-checkpoint";
  }
  if (row.scope === "workspace" && row.scope_id && row.state_key === "historical-import") {
    assertString(row.scope_id);
    return "historical-import-checkpoint";
  }
  fail("plugin state row is outside the approved checkpoint namespaces");
}

function parseJSON(source) {
  if (typeof source !== "string") fail("state row is not JSON text");
  try {
    return JSON.parse(source);
  } catch {
    fail("state row contains malformed JSON");
  }
}

function validateRow(row, schema, markers) {
  const outer = parseJSON(row.value_json);
  scanPrivateMarkers([row.scope, row.scope_id, row.state_key, outer], markers);
  switch (schema) {
    case "privacy-migration-checkpoint":
      validatePrivacyMigration(parseWrappedJSON(outer));
      break;
    case "usage-observation-receipt":
      validateUsageReceipt(parseWrappedJSON(outer), row);
      break;
    case "usage-target-checkpoint":
      validateUsageTarget(parseWrappedJSON(outer), row);
      break;
    case "tokscale-membership-checkpoint":
      validateMembership(parseWrappedJSON(outer), row);
      break;
    case "collector-checkpoint":
      validateCollector(outer);
      break;
    case "historical-import-checkpoint":
      validateImport(outer);
      break;
    default:
      fail("plugin state schema is unsupported");
  }
}

export function assertSafeInspectionTarget({ tmpDir, databaseFilename, readOnly }) {
  if (readOnly !== true) fail("database access must be read-only");
  if (typeof tmpDir !== "string" || !path.isAbsolute(tmpDir) || typeof databaseFilename !== "string") {
    fail("an explicit disposable directory and database filename are required");
  }
  if (!allowedDatabaseNames.has(databaseFilename)) fail("database filename is not an approved E2E fixture database");

  let tempRoot;
  let realTmpDir;
  try {
    tempRoot = fs.realpathSync(os.tmpdir());
    realTmpDir = fs.realpathSync(tmpDir);
  } catch {
    fail("disposable E2E directory is unavailable");
  }
  if (
    path.resolve(tmpDir) !== realTmpDir ||
    path.dirname(realTmpDir) !== tempRoot ||
    !/^kandev-e2e-\d+-.+$/.test(path.basename(realTmpDir))
  ) {
    fail("database directory is not a direct disposable Kandev E2E fixture root");
  }

  const databasePath = path.join(realTmpDir, databaseFilename);
  let databaseStat;
  let realDatabasePath;
  try {
    databaseStat = fs.lstatSync(databasePath);
    realDatabasePath = fs.realpathSync(databasePath);
  } catch {
    fail("approved E2E database file is unavailable");
  }
  if (!databaseStat.isFile() || databaseStat.isSymbolicLink() || realDatabasePath !== databasePath) {
    fail("database file is not a canonical regular file inside the disposable root");
  }
  return databasePath;
}

export function inspectRetainedUsagePluginState({
  tmpDir,
  databaseFilename,
  readOnly = true,
  forbiddenValues = [],
}) {
  const databasePath = assertSafeInspectionTarget({ tmpDir, databaseFilename, readOnly });
  if (!Array.isArray(forbiddenValues) || forbiddenValues.some((value) => typeof value !== "string")) {
    fail("private fixture markers must be strings");
  }
  const markers = forbiddenValues.map((value) => value.trim()).filter(Boolean);
  let database;
  try {
    database = new DatabaseSync(databasePath, { readOnly: true });
    database.exec("PRAGMA query_only = ON");
    const queryOnly = database.prepare("PRAGMA query_only").get();
    if (!queryOnly || queryOnly.query_only !== 1) fail("SQLite query-only mode could not be enforced");
    const hasTable = database
      .prepare("SELECT 1 AS found FROM sqlite_master WHERE type = 'table' AND name = 'plugin_state'")
      .get();
    if (!hasTable) fail("Host database has no plugin state table");
    const rows = database
      .prepare(`
        SELECT scope, scope_id, state_key, value_json
        FROM plugin_state
        WHERE plugin_id = ?
        ORDER BY scope, scope_id, state_key
      `)
      .all(SESSION_COST_PLUGIN_ID);
    if (rows.length === 0) fail("installed plugin has no persisted state rows to inspect");

    const counts = Object.fromEntries(schemaNames.map((name) => [name, 0]));
    for (const row of rows) {
      const schema = schemaForRow(row);
      validateRow(row, schema, markers);
      counts[schema]++;
    }
    return Object.freeze({
      pluginId: SESSION_COST_PLUGIN_ID,
      databaseFilename,
      rowCount: rows.length,
      schemaCounts: Object.freeze(counts),
    });
  } catch (error) {
    if (error instanceof Error && error.message.startsWith("Retained usage plugin-state privacy inspection failed:")) {
      throw error;
    }
    fail("read-only inspection of the disposable Host database failed");
  } finally {
    database?.close();
  }
}
