#!/usr/bin/env python3
"""Mutation check: break one guard at a time, assert its test catches it.

    python3 hack/mutation-check.py            # all mutations
    python3 hack/mutation-check.py session    # one group
    python3 hack/mutation-check.py -l         # list without running

Why this exists
---------------
A test that passes the first time it is written has demonstrated nothing
— it has only shown that the code and the test agree today. Every guard
in this repo exists because something went wrong in production once, or
is close enough to the shape of something that has, so each one gets an
entry below that breaks it deliberately and asserts the corresponding
test goes red with a message a human can act on.

Three failure modes this harness reports, which are easy to confuse:

  NOT CAUGHT    the test passes with the guard broken. The test is
                decorative. Fix the test.
  INVALID       the mutated code does not compile (typically an
                unused variable), so nothing was proven either way.
                Fix the mutation, not the test.
  HUNG          the test deadlocks instead of failing. Worse than no
                test: it burns the whole CI timeout and reports
                nothing. Fix the test's synchronisation.

It covers BOTH the service and plugin modules. The two mandatory
version-wire entries — `/healthz wired without the version` and
`session wired without the version` — are the ones whose shape has
cost this fleet twice already: a value present, correct, and read by
nothing, logging identically to a working deploy (`signingKey` as a
bare ARN on backup-smoke-1d 2026-11-14; NOTIFIER_OPERATOR_PASSWORD on
the first backup.simple-forge.com login 2026-11-18). Neither is
observable from a handler-level test — every test in internal/api
constructs its own handler, so all of them pass with main.go never
passing `version` at all. Hence the binary test, and hence the two
entries that assert the two independent wires.

One thing to watch for when ADDING a mutation: the harness runs
`go test <pkg> -run <test>` in the Go module that owns the file. If
that module is also the one where the TEST lives, a bare package works;
if it isn't, write "<module>:<pkg>" (the cross-module shape). A test
name that does not exist in the target package produces "no tests to
run" and exits 0, which reports as NOT CAUGHT rather than as a
mistake in the entry. Observatory's binary version-wire test lives in
service/cmd/observatory/, NOT in service/internal/api/, so the two
session+healthz entries both target SVC_CMD_PKG even though one of
them mutates a file under internal/api/. Live-caught twice while
writing this file.

The working tree is restored after every mutation, including on failure
and on Ctrl-C. Run it on a clean tree anyway: a crash mid-write would
leave one file mutated, and `git diff` is the check.
"""

import pathlib
import shutil
import signal
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The repo is three Go modules; mutations below live in two of them
# (api/ holds no guards worth breaking — it is types only). File paths
# below are REPO-relative and their first segment names the module,
# which is also the directory `go test` runs in.

API_PKG = "./internal/api/"
PLUGIN_PKG = "./internal/"
# The service module's cmd package — holds the binary version-wire
# test. Easy to confuse with PLG_CMD_PKG below: both are "the cmd
# package", and reaching for the wrong one does not fail loudly. The
# mutation still applies (the find string is in the file), the harness
# runs a test package in the OTHER module where that test name does
# not exist, `go test -run` reports "no tests to run" and exits 0, and
# the entry lands as NOT CAUGHT rather than as a mistake in the entry.
SVC_CMD_PKG = "./cmd/observatory/"
PLG_CMD_PKG = "./cmd/sc-component-observatory/"

SESSION = "service/internal/api/session.go"
HTTPMW = "service/internal/api/http.go"
SERVICES = "service/internal/api/services.go"
HEALTH = "service/internal/api/health.go"
MIGRATION = "service/internal/api/migration.go"
SVC_MAIN = "service/cmd/observatory/main.go"

PLG_CONFIG = "plugin/internal/config.go"
PLG_TENANTENV = "plugin/internal/tenant_env.go"
PLG_MAIN = "plugin/cmd/sc-component-observatory/main.go"

# (group, name, file, test-package, find, replace, test-regex, occurrences)
MUTATIONS = [
    # --------------------------------------------------------------- session
    # The build stamp, end to end: ldflags -> main.version -> the JSON on
    # /healthz (what a smoke reads with no credential) and on
    # /api/v1/session (what the SPA header renders). Both wires are in
    # main.go and the session JSON write is in session.go, and NEITHER
    # is visible to a handler test: every test in internal/api
    # constructs its own handler, so all of them pass with main.go
    # never passing `version` at all. That is the exact shape of the
    # bug this fleet has shipped twice. Hence the binary test, and
    # hence these two entries — one per wire. Mirrors notifier#6.
    #
    # Target SVC_CMD_PKG for both, because the only test that observes
    # either is the binary test, which lives there — not in API_PKG.
    ("session", "healthz wire drops the version", SVC_MAIN, SVC_CMD_PKG,
     'fmt.Fprintf(w, `{"status":"ok","version":%q,"commit":%q}`, version, commit)',
     'fmt.Fprintf(w, `{"status":"ok","version":%q,"commit":%q}`, "", commit)',
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    ("session", "session wire drops the version", SESSION, SVC_CMD_PKG,
     '"version": c.Version,',
     '"version": "",',
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    # ------------------------------------------------------------------ auth
    # The operator gate on the dashboard's read routes. Validity alone
    # is not enough and this is not theoretical: EVERY consumer stack's
    # plugin holds a valid `register` token for this service. A read
    # route that accepted any valid token would let any one tenant
    # read every other tenant's record — including the full sanitized
    # client config the detail view serves and the per-deploy diffs
    # built from it.
    ("auth", "requireOperator checks validity, not purpose", HTTPMW, API_PKG,
     "if claims == nil || claims.Op != OpOperator {", "if claims == nil {",
     "TestLogs_RequiresOperator", 1),

    # The scheduler-or-operator gate on the tick. Register tokens must
    # be refused: the sweep makes an outbound HTTP request to every
    # endpoint in the registry, so a tick that accepted any valid
    # token would let any single consumer stack drive traffic at the
    # whole fleet on demand.
    ("auth", "healthcheck tick accepts any valid token", HEALTH, API_PKG,
     "(claims.Op != OpScheduler && claims.Op != OpOperator)",
     "false",
     "TestTick_RequiresSchedulerOrOperatorOp", 1),

    # Register must demand the `register` op specifically. An operator
    # session token must not also be usable as a push-register token,
    # or an SPA bug that reflects the session cookie into a POST
    # /services/register call creates rows without ever reaching the
    # plugin side of the contract.
    ("auth", "register accepts any valid op", SERVICES, API_PKG,
     "if claims == nil || claims.Op != OpRegister {",
     "if claims == nil {",
     "TestRegister_RejectsWrongOp", 1),

    # The whole point of a per-consumer token: the register handler
    # takes the stack identity from the body AND the JWT's `tenant`
    # claim, and refuses the mismatch. One consumer must not be able
    # to register under another's name using its own token.
    ("auth", "register accepts cross-tenant token", SERVICES, API_PKG,
     "if claims.Tenant != body.Stack {",
     "if false {",
     "TestRegister_RejectsCrossTenantToken", 1),

    # ------------------------------------------------------------- migration
    # The S3 → go-embedb migration is NOT behind the service JWT — it
    # is gated by ADMIN_MIGRATION_TOKEN on a separate header. The
    # opt-in gate at the top of Migrate is the entire difference
    # between "route not mounted" and "route open to anyone":
    # removing it opens the endpoint to a request with NO token at
    # all, because a wrong-config `AdminToken == ""` and a missing
    # header compare equal.
    ("migration", "opt-in gate removed", MIGRATION, API_PKG,
     '\tif c.AdminToken == "" {\n'
     '\t\twriteErr(w, http.StatusNotFound, "migration endpoint disabled (ADMIN_MIGRATION_TOKEN unset)")\n'
     "\t\treturn\n\t}\n",
     "",
     "TestMigration_AuthAndGate", 1),

    # The token compare must distinguish match from mismatch. Flipping
    # the result sign turns "wrong token" into "right token" and
    # hands the migration endpoint to anyone sending any header value.
    ("migration", "admin token compare accepts anything", MIGRATION, API_PKG,
     "if subtle.ConstantTimeCompare([]byte(got), []byte(c.AdminToken)) != 1 {",
     "if subtle.ConstantTimeCompare([]byte(got), []byte(c.AdminToken)) == -1 {",
     "TestMigration_AuthAndGate", 1),

    # ---------------------------------------------------------------- plugin
    # signingKey is the HS256 secret the service validates registration
    # tokens with — if the plugin accepts an empty one, every consumer
    # of this component silently signs with "" and the service answers
    # every registration with 401. A deploy-green outcome in which
    # nothing registers.
    ("plugin", "blank signingKey accepted", PLG_CONFIG, PLUGIN_PKG,
     '\tif v, ok := configRaw(input)["signingKey"].(string); ok && v != "" {',
     '\tif v, ok := configRaw(input)["signingKey"].(string); ok || v == "" {',
     "TestReadSigningKey_ErrorNamesTheFix", 1),

    # ------------------------------------------------------- plugin/tenantenv
    # Empty allowlist MUST allow everything — the pre-filter behaviour.
    # Failing closed here would stop every registration in the fleet
    # the day an operator adds this key as a typo, with the dashboard
    # going silent at the exact moment the operator would least expect
    # it to.
    ("plugin", "empty allowlist locks everything out", PLG_TENANTENV, PLUGIN_PKG,
     "\tif len(allowed) == 0 {\n\t\treturn true\n\t}",
     "\tif len(allowed) == 0 {\n\t\treturn false\n\t}",
     "TestEnvMayRegister_EmptyAllowsAll", 1),

    # Prefix matching would admit `staging-ru` on a `staging` allowlist
    # — a real and separate environment here. Silent, and catastrophic
    # when a stack cloned under a prefix of an allowlisted name starts
    # registering alongside the legitimate one.
    ("plugin", "environment allowlist matches by prefix", PLG_TENANTENV, PLUGIN_PKG,
     "\t\tif strings.TrimSpace(a) == env {",
     "\t\tif strings.HasPrefix(env, strings.TrimSpace(a)) {",
     "TestEnvMayRegister_NoPrefixMatching", 1),

    # An empty env against a non-empty allowlist is a resolution
    # failure, not a match. Admitting it would let a dispatch shape
    # that loses the env register anyway, as a tenant with no
    # environment at all.
    ("plugin", "empty env admitted into a non-empty allowlist", PLG_TENANTENV, PLUGIN_PKG,
     '\tenv = strings.TrimSpace(env)\n\tif env == "" {\n\t\treturn false\n\t}',
     '\tenv = strings.TrimSpace(env)\n\tif env == "" {\n\t\treturn true\n\t}',
     "TestEnvMayRegister_EmptyEnvIsRefused", 1),

    # ------------------------------------------------------- secret-env wire
    # Schema() is the component's published API and it is two plain
    # maps with nothing keeping them consistent. Both ways they drift
    # are silent at deploy time.
    #
    # Core dual-uses ComponentAllowedOverrides as the allowed-top-
    # level-keys check, so a bound key that is not on it is refused at
    # PARSE time with "unknown or disallowed top-level key" — the
    # operator cannot write the line, and the binding can never fire.
    ("plugin", "secret bound to a key the operator cannot write", PLG_MAIN, PLG_CMD_PKG,
     '\t\t\t\t"migrationToken",\n', "",
     "TestMigrationTokenIsSecretBound", 1),

    # Two config keys binding to one env var: one would silently win,
    # and which one is implementation-dependent. Both config keys land
    # as plaintext placeholders in the operator's config either way,
    # and the one that loses just stops controlling anything.
    ("plugin", "two config keys bound to one env var", PLG_MAIN, PLG_CMD_PKG,
     '"migrationToken":   "ADMIN_MIGRATION_TOKEN",',
     '"migrationToken":   "SERVICE_JWT_SECRET",',
     "TestSecretEnvVarNamesAreSane", 1),

    # The one that has actually happened in this fleet, twice. A bound
    # secret arrives as a SecretsManager ARN; a service that reads it
    # with a bare os.Getenv compares against the ARN string forever,
    # logs nothing, and looks exactly like a working deploy.
    # Live-caught on signingKey (backup-smoke-1d 2026-11-14) and
    # NOTIFIER_OPERATOR_PASSWORD (backup.simple-forge.com 2026-11-18).
    ("plugin", "bound secret read as raw env by the service", SVC_MAIN, "plugin:" + PLG_CMD_PKG,
     '\tmigrationToken, err := resolveAWSSecretARN(ctx, "ADMIN_MIGRATION_TOKEN", os.Getenv("ADMIN_MIGRATION_TOKEN"))',
     "\tmigrationToken, err := os.Getenv(\"ADMIN_MIGRATION_TOKEN\"), error(nil)",
     "TestSecretEnvVarsAreResolvedByTheService", 1),
]

TEST_FILE_PREFIXES = (
    "services_test.go:", "deploys_test.go:", "health_tick_auth_test.go:",
    "metrics_test.go:", "logs_test.go:", "migration_test.go:",
    "config_test.go:", "tenant_env_test.go:",
    "schema_test.go:", "version_wire_test.go:",
)
COMPILE_MARKERS = ("build failed", "undefined:", "declared and not used", "cannot use")


def first_message(out: str) -> tuple[str, bool]:
    """Return (message, is_compile_error) from a go test failure.

    A t.Fatalf message sits on the same line as the file:line prefix, but
    gomega puts nothing there and wraps its "Expected / to equal" report
    across the following indented lines. Both shapes have to read as one
    sentence here, or the api-package mutations report a bare line number
    and the operator has to re-run the test by hand to learn what broke.
    """
    lines = out.splitlines()
    for i, line in enumerate(lines):
        s = line.strip()
        if s.startswith(TEST_FILE_PREFIXES):
            msg = s.split(": ", 1)[-1] if ": " in s else ""
            if msg:
                return msg, False
            tail = [t.strip() for t in lines[i + 1:i + 7]
                    if t.strip() and not t.startswith(("FAIL", "ok", "---", "===", "exit status"))]
            return " ".join(tail) or s, False
        if any(m in s for m in COMPILE_MARKERS):
            return s, True
    return "", False


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("-")]
    listing = "-l" in sys.argv or "--list" in sys.argv

    muts = [m for m in MUTATIONS if not args or m[0] in args]
    if not muts:
        groups = sorted({m[0] for m in MUTATIONS})
        print(f"no mutations match {args}; groups are: {', '.join(groups)}")
        return 2

    if listing:
        for group, name, _, _, _, _, test, _ in muts:
            print(f"{group:9} {name:48} {test}")
        return 0

    # SIGTERM does not raise, so the restore in `finally` below never runs
    # and the tree is left with exactly one file mutated — which then shows
    # up as an unrelated failure in whatever runs next. Turning it into
    # SystemExit makes the restore happen.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(130))

    # Back up exactly the files this run will mutate, rather than each
    # module's whole internal/ tree. Guards worth mutating also live
    # OUTSIDE internal/ — the schema allowlist and secret-env map in
    # plugin/cmd/, the secret resolve in service/cmd/ — and with a
    # tree copy keyed on internal/ the restore below raises
    # FileNotFoundError for those and leaves the file mutated. Backing
    # up by path covers both, and the harness only ever writes files
    # named in MUTATIONS, so it loses no insurance.
    backup = pathlib.Path(tempfile.mkdtemp(prefix="mutation-check-"))
    mutated_files = sorted({m[2] for m in muts})
    for rel in mutated_files:
        (backup / rel).parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / rel, backup / rel)

    problems = []
    try:
        for group, name, rel, pkg, find, repl, test, want in muts:
            label = f"{group}/{name}"
            path = ROOT / rel
            # The module the file belongs to is normally also the
            # directory the test has to run in: each module has its own
            # go.mod, so `go test ./internal/` resolves differently per
            # module. The exception is a cross-module contract test (the
            # plugin asserting something about the service's source),
            # which names its module as "<module>:<pkg>".
            if ":" in pkg:
                mod_name, pkg = pkg.split(":", 1)
            else:
                mod_name = rel.split("/", 1)[0]
            module = ROOT / mod_name
            src = path.read_text()
            if src.count(find) != want:
                print(f"STALE       {label}")
                print(f"              pattern occurs {src.count(find)}x, expected {want} — "
                      f"the code moved; update the mutation")
                problems.append(label)
                continue

            path.write_text(src.replace(find, repl))
            try:
                proc = subprocess.run(
                    ["go", "test", pkg, "-count=1", "-run", test],
                    cwd=module, capture_output=True, text=True, timeout=180,
                )
            except subprocess.TimeoutExpired:
                print(f"HUNG        {label}")
                print("              the test deadlocks instead of failing — it would burn the CI timeout")
                problems.append(label)
                continue
            finally:
                shutil.copyfile(backup / rel, path)

            if proc.returncode == 0:
                print(f"NOT CAUGHT  {label}")
                print(f"              {test} PASSES with the guard broken — it proves nothing")
                problems.append(label)
                continue

            msg, compile_err = first_message(proc.stdout + proc.stderr)
            if compile_err:
                print(f"INVALID     {label}")
                print(f"              the mutation does not compile, so nothing was proven: {msg}")
                problems.append(label)
                continue

            print(f"caught      {label}")
            if msg:
                print(f"              → {msg[:160]}")
    finally:
        # Belt and braces: restore every file this run could have
        # touched, so a crash or a Ctrl-C mid-run cannot leave a mutated
        # file behind.
        for rel in mutated_files:
            shutil.copyfile(backup / rel, ROOT / rel)
        shutil.rmtree(backup, ignore_errors=True)

    print()
    if problems:
        print(f"{len(problems)} of {len(muts)} not proven:")
        for p in problems:
            print(f"  - {p}")
        return 1
    print(f"all {len(muts)} mutations caught")
    return 0


if __name__ == "__main__":
    sys.exit(main())
