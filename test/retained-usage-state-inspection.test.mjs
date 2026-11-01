import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
  assertSafeInspectionTarget,
  inspectRetainedUsagePluginState,
  SESSION_COST_AUTH_DATABASE,
  SESSION_COST_DEFAULT_DATABASE,
  SESSION_COST_PLUGIN_ID,
} from "./retained-usage-state-inspection.mjs";

const sqlite = process.getBuiltinModule("node:sqlite");
const { DatabaseSync } = sqlite;
const digest = `sha256:${"a".repeat(64)}`;

function fixture(t, records = [], databaseFilename = SESSION_COST_DEFAULT_DATABASE) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), `kandev-e2e-${process.pid}-`));
  const databasePath = path.join(tmpDir, databaseFilename);
  const database = new DatabaseSync(databasePath);
  database.exec(`
    CREATE TABLE plugin_state (
      id TEXT PRIMARY KEY,
      plugin_id TEXT NOT NULL,
      scope TEXT NOT NULL DEFAULT 'instance',
      scope_id TEXT NOT NULL DEFAULT '',
      state_key TEXT NOT NULL,
      value_json TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      UNIQUE (plugin_id, scope, scope_id, state_key)
    )
  `);
  const insert = database.prepare(`
    INSERT INTO plugin_state (id, plugin_id, scope, scope_id, state_key, value_json, updated_at)
    VALUES (?, ?, ?, ?, ?, ?, ?)
  `);
  for (const [index, row] of records.entries()) {
    insert.run(
      `row-${index}`,
      row.pluginId ?? SESSION_COST_PLUGIN_ID,
      row.scope,
      row.scopeId,
      row.stateKey,
      typeof row.value === "string" ? row.value : JSON.stringify(row.value),
      "2026-10-30T12:00:00Z",
    );
  }
  database.close();
  t.after(() => fs.rmSync(tmpDir, { recursive: true, force: true }));
  return { tmpDir, databasePath, databaseFilename };
}

function safeRows() {
  const importState = {
    run_id: "run-opaque-123",
    schema_version: 3,
    snapshot_id: "snapshot-opaque-123",
    snapshot_upper_bound: "1200",
    phase: "daily",
    scan_cursor: "cursor-opaque-123",
    intervals: [{ start: "2026-10-01", end: "2026-10-30" }],
    next_day: "2026-10-30",
    days_total: 30,
    days_done: 29,
    day_attempts: 1,
    day_seconds_total: "420",
    workspace_id: "workspace-opaque-123",
    status: "completed",
    sessions_found: 150,
    sessions_imported: 149,
    sessions_missing: 0,
    sessions_skipped: 1,
    started_at: "2026-10-30T12:00:00Z",
    finished_at: "2026-10-30T12:07:00Z",
    last_successful_at: "2026-10-30T12:07:00Z",
    undated: false,
  };
  return [
    {
      scope: "plugin",
      scopeId: "usage-state-privacy-v2",
      stateKey: "completed",
      value: { json: JSON.stringify({ Version: 2, Complete: true }) },
    },
    {
      scope: "plugin",
      scopeId: "usage-observation-v1",
      stateKey: digest,
      value: {
        json: JSON.stringify({
          Version: 2,
          ContentDigest: digest,
          SubjectBinding: "binding-opaque-123",
          ItemID: digest,
          Revision: 1200,
          PayloadDigest: digest,
          Accepted: true,
        }),
      },
    },
    {
      scope: "workspace",
      scopeId: "workspace-opaque-123",
      stateKey: "historical-import",
      value: importState,
    },
    {
      scope: "plugin",
      scopeId: "historical-import",
      stateKey: "import-queue",
      value: { workspace_ids: ["workspace-opaque-123"] },
    },
    {
      scope: "plugin",
      scopeId: "collector",
      stateKey: "checkpoint",
      value: {
        last_date: "2026-10-30",
        last_scan_at: "2026-10-30T12:00:00Z",
        initialized: true,
        pending: {
          "session-opaque-123": {
            subject_binding: "binding-opaque-123",
            task_id: "task-opaque-123",
            workspace_id: "workspace-opaque-123",
            acp_session_id: "transcript-opaque-123",
            state: "COMPLETED",
            skipped: true,
          },
        },
      },
    },
    {
      pluginId: "another-plugin",
      scope: "plugin",
      scopeId: "foreign-private-state",
      stateKey: "foreign-key",
      value: { Tokens: { InputTokens: 987654321 } },
    },
  ];
}

test("inspects allowlisted checkpoints without treating scheduling counts as usage amounts", (t) => {
  const db = fixture(t, safeRows());
  const before = createHash("sha256").update(fs.readFileSync(db.databasePath)).digest("hex");
  const result = inspectRetainedUsagePluginState({
    tmpDir: db.tmpDir,
    databaseFilename: db.databaseFilename,
    readOnly: true,
    forbiddenValues: ["Private retained task title", "retained-private-model"],
  });
  const after = createHash("sha256").update(fs.readFileSync(db.databasePath)).digest("hex");

  assert.equal(result.pluginId, SESSION_COST_PLUGIN_ID);
  assert.equal(result.rowCount, 5);
  assert.equal(result.schemaCounts["historical-import-checkpoint"], 1);
  assert.equal(result.schemaCounts["historical-import-queue"], 1);
  assert.equal(result.schemaCounts["usage-observation-receipt"], 1);
  assert.equal(before, after, "inspection must not modify the disposable database");
  assert.doesNotMatch(JSON.stringify(result), /binding-opaque|workspace-opaque|Private retained/);
});

test("validates the persisted historical import queue namespace", (t) => {
  const queue = {
    scope: "plugin",
    scopeId: "historical-import",
    stateKey: "import-queue",
    value: { workspace_ids: ["workspace-opaque-123"] },
  };
  const accepted = fixture(t, [queue]);
  assert.equal(
    inspectRetainedUsagePluginState({ tmpDir: accepted.tmpDir, databaseFilename: accepted.databaseFilename })
      .schemaCounts["historical-import-queue"],
    1,
  );

  for (const invalidValue of [
    { workspace_ids: "workspace-opaque-123" },
    { workspace_ids: ["workspace-opaque-123", 42] },
    { workspace_ids: ["workspace-opaque-123"], tokens: 0 },
  ]) {
    const db = fixture(t, [{ ...queue, value: invalidValue }]);
    assert.throws(
      () => inspectRetainedUsagePluginState({ tmpDir: db.tmpDir, databaseFilename: db.databaseFilename }),
      /historical import queue is invalid|checkpoint contains an invalid string value|prohibited accounting or personal-data field/,
    );
  }
});

test("validates the persisted skipped flag on collector checkpoints", (t) => {
  const rows = safeRows();
  const collector = rows.find((row) => row.scopeId === "collector");
  const pending = collector.value.pending["session-opaque-123"];
  pending.skipped = "true";
  const db = fixture(t, rows);

  assert.throws(
    () => inspectRetainedUsagePluginState({ tmpDir: db.tmpDir, databaseFilename: db.databaseFilename }),
    /checkpoint contains an invalid boolean value/,
  );
});

test("accepts the authenticated smoke's explicit canonical database filename", (t) => {
  const db = fixture(t, [{
    scope: "plugin",
    scopeId: "usage-state-privacy-v2",
    stateKey: "completed",
    value: { json: JSON.stringify({ Version: 2, Complete: true }) },
  }], SESSION_COST_AUTH_DATABASE);
  const result = inspectRetainedUsagePluginState({
    tmpDir: db.tmpDir,
    databaseFilename: SESSION_COST_AUTH_DATABASE,
    readOnly: true,
  });
  assert.equal(result.databaseFilename, SESSION_COST_AUTH_DATABASE);
});

test("rejects accounting payload fields by schema, regardless of their numeric values", (t) => {
  const db = fixture(t, [{
    scope: "plugin",
    scopeId: "usage-observation-v1",
    stateKey: digest,
    value: {
      json: JSON.stringify({
        Version: 2,
        ContentDigest: digest,
        SubjectBinding: "binding-opaque-123",
        ItemID: digest,
        Revision: 1,
        PayloadDigest: digest,
        Accepted: true,
        Tokens: { InputTokens: 987654321 },
      }),
    },
  }]);

  assert.throws(
    () => inspectRetainedUsagePluginState({ tmpDir: db.tmpDir, databaseFilename: db.databaseFilename }),
    (error) => {
      assert.match(error.message, /prohibited accounting or personal-data field/);
      assert.doesNotMatch(error.message, /987654321|InputTokens/);
      return true;
    },
  );
});

test("rejects prompt and title fields without printing checkpoint JSON", (t) => {
  for (const privateField of ["task_title", "prompt_text"]) {
    const db = fixture(t, [{
      scope: "workspace",
      scopeId: "workspace-opaque-123",
      stateKey: "historical-import",
      value: {
        workspace_id: "workspace-opaque-123",
        schema_version: 3,
        status: "completed",
        [privateField]: "private fixture prose that must not be printed",
      },
    }]);
    assert.throws(
      () => inspectRetainedUsagePluginState({ tmpDir: db.tmpDir, databaseFilename: db.databaseFilename }),
      (error) => {
        assert.match(error.message, /prohibited accounting or personal-data field/);
        assert.doesNotMatch(error.message, /private fixture prose/);
        return true;
      },
    );
  }
});

test("rejects supplied private markers in otherwise allowed string fields", (t) => {
  const marker = "PrivateRetainedTitleMarker";
  const db = fixture(t, [{
    scope: "workspace",
    scopeId: "workspace-opaque-123",
    stateKey: "historical-import",
    value: {
      run_id: marker,
      workspace_id: "workspace-opaque-123",
      schema_version: 3,
      status: "completed",
    },
  }]);

  assert.throws(
    () => inspectRetainedUsagePluginState({
      tmpDir: db.tmpDir,
      databaseFilename: db.databaseFilename,
      forbiddenValues: [marker],
    }),
    (error) => {
      assert.match(error.message, /supplied private fixture marker/);
      assert.doesNotMatch(error.message, /PrivateRetainedTitleMarker/);
      return true;
    },
  );
});

test("rejects unsafe roots, filenames, symlinks, and requests for writable access", (t) => {
  const db = fixture(t);
  const target = {
    tmpDir: db.tmpDir,
    databaseFilename: db.databaseFilename,
    readOnly: true,
  };
  assert.equal(assertSafeInspectionTarget(target), db.databasePath);
  assert.throws(() => assertSafeInspectionTarget({ ...target, readOnly: false }), /read-only/);
  assert.throws(() => assertSafeInspectionTarget({ ...target, databaseFilename: "foreign.db" }), /filename/);
  assert.throws(() => assertSafeInspectionTarget({ ...target, databaseFilename: db.databasePath }), /filename/);
  assert.throws(
    () => inspectRetainedUsagePluginState({ tmpDir: db.tmpDir }),
    /explicit disposable directory and database filename/,
  );
  assert.throws(
    () => assertSafeInspectionTarget({ ...target, tmpDir: path.relative(process.cwd(), db.tmpDir) }),
    /explicit disposable directory/,
  );

  const nonDisposable = fs.mkdtempSync(path.join(os.tmpdir(), "not-kandev-e2e-"));
  t.after(() => fs.rmSync(nonDisposable, { recursive: true, force: true }));
  assert.throws(() => assertSafeInspectionTarget({ ...target, tmpDir: nonDisposable }), /disposable/);

  const foreign = path.join(nonDisposable, "foreign.db");
  fs.copyFileSync(db.databasePath, foreign);
  const link = path.join(db.tmpDir, "kandev-auth-usage-history.db");
  fs.symlinkSync(foreign, link);
  assert.throws(
    () => assertSafeInspectionTarget({ ...target, databaseFilename: "kandev-auth-usage-history.db" }),
    /regular file inside the disposable root/,
  );
});

test("refuses plugin-state records outside the approved checkpoint key policy", (t) => {
  const db = fixture(t, [{
    scope: "plugin",
    scopeId: "canonical-usage-v1",
    stateKey: "legacy",
    value: { json: "{}" },
  }]);
  assert.throws(
    () => inspectRetainedUsagePluginState({ tmpDir: db.tmpDir, databaseFilename: db.databaseFilename }),
    /legacy canonical accounting state remains/,
  );
});
