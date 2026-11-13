#!/usr/bin/env sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
FIXTURE_DIR="$REPO_ROOT/testdata/kvfixture"
BIN_DIR="$REPO_ROOT/bin"
FIXTURE_BIN_DIR="$FIXTURE_DIR/bin"

echo "=== [1/5] Building synthesis CLI, loadgen, and linearizable-kv ==="
mkdir -p "$BIN_DIR" "$FIXTURE_BIN_DIR"
(
  cd "$REPO_ROOT"
  go build -trimpath -ldflags="-s -w" -o "$BIN_DIR/synthesis" ./cmd/synthesis
  go build -trimpath -ldflags="-s -w" -o "$FIXTURE_BIN_DIR/linearizable-kv" ./cmd/synthesis-oracle-linearizable
)
(
  cd "$FIXTURE_DIR"
  go build -trimpath -ldflags="-s -w" -o "$FIXTURE_BIN_DIR/loadgen" ./cmd/loadgen
)

echo "=== [2/5] Building and smoke-booting kvfixture images (buggy & kvfixed) ==="
for variant in buggy kvfixed; do
  (
    cd "$FIXTURE_DIR"
    KV_VARIANT="$variant" KV_PREFIX="syn_smoke_$variant" docker compose build kv-n1
    KV_VARIANT="$variant" KV_PREFIX="syn_smoke_$variant" docker compose up --build -d
    KV_VARIANT="$variant" KV_PREFIX="syn_smoke_$variant" docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  )
done

SYNTHESIS_BIN="$BIN_DIR/synthesis"

echo "=== Verifying tamper-evident test lock (.synthesis/lock) ==="
"$SYNTHESIS_BIN" verify-lock --project "$FIXTURE_DIR"

echo ""
echo "=== [3/5] Scenario 1 (AT-1): Defective Raft read-lease caught with witness ==="
set +e
"$SYNTHESIS_BIN" run --project "$FIXTURE_DIR" --profile buggy --seed 20261109
AT1_CODE=$?
set -e
if [ "$AT1_CODE" -ne 1 ]; then
  echo "AT-1 expected exit code 1 (FAIL), got $AT1_CODE" >&2
  exit 1
fi
LATEST_RUN=$(ls -1dt "$FIXTURE_DIR/.synthesis/runs"/r_* 2>/dev/null | head -n 1 || true)
if [ -n "$LATEST_RUN" ] && [ -f "$LATEST_RUN/verdict.json" ]; then
  "$SYNTHESIS_BIN" explain "$LATEST_RUN/verdict.json"
fi

echo ""
echo "=== [4/5] Scenario 2 (AT-2): Fixed Raft quorum-read passes full locked budget ==="
"$SYNTHESIS_BIN" run --project "$FIXTURE_DIR" --profile patched --seed 20261109
AT2_CODE=$?

echo ""
echo "=== [5/5] Scenario 3 (AT-3): Quiet workload weakening refused before boot ==="
TAMPER_DIR=$(mktemp -d "${TMPDIR:-/tmp}/synthesis-tamper.XXXXXX")
cleanup_tamper() {
  rm -rf "$TAMPER_DIR"
}
trap cleanup_tamper EXIT

mkdir -p "$TAMPER_DIR/.synthesis/oracles"
cp "$FIXTURE_DIR/synthesis.yaml" "$TAMPER_DIR/synthesis.yaml"
cp "$FIXTURE_DIR/docker-compose.yaml" "$TAMPER_DIR/docker-compose.yaml"
cp "$FIXTURE_DIR/.synthesis/lock" "$TAMPER_DIR/.synthesis/lock"
cp "$FIXTURE_DIR/.synthesis/oracles/linearizable.kv.yaml" "$TAMPER_DIR/.synthesis/oracles/linearizable.kv.yaml"
sed 's/ops: 60000/ops: 100/' "$FIXTURE_DIR/synthesis.yaml" > "$TAMPER_DIR/synthesis.yaml"

set +e
"$SYNTHESIS_BIN" run --project "$TAMPER_DIR" --profile buggy
AT3_CODE=$?
set -e
if [ "$AT3_CODE" -ne 4 ]; then
  echo "AT-3 expected exit code 4 (ORACLE_DRIFT), got $AT3_CODE" >&2
  exit 1
fi

echo ""
echo "=== DEMO SUMMARY ==="
echo "  Scenario 1 (AT-1, defective fixture): FAIL         (exit $AT1_CODE) - witnessed stale read + witness.svg"
echo "  Scenario 2 (AT-2, fixed fixture):     PASS         (exit $AT2_CODE) - full locked budget consumed"
echo "  Scenario 3 (AT-3, weakened workload): ORACLE_DRIFT (exit $AT3_CODE) - refused before cluster boot"
