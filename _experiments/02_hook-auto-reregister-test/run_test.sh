#!/bin/bash
# ============================================================
# auto-reregister hook simulation test
#
# Sets up a mock environment and runs the real run.sh to verify
# each step works correctly without a live API or real registration.
# ============================================================

set -euo pipefail

TEST_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK_DIR="$(cd "$TEST_DIR/../../hook-scripts/auto-reregister" && pwd)"
MOCK_PORT=18787
MOCK_API_PID=""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

PASS_COUNT=0
FAIL_COUNT=0

pass() { PASS_COUNT=$((PASS_COUNT + 1)); echo -e "  ${GREEN}✓ PASS${NC}: $1"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); echo -e "  ${RED}✗ FAIL${NC}: $1"; }

cleanup() {
    echo ""
    echo -e "${CYAN}[cleanup]${NC} Cleaning up..."
    if [ -n "$MOCK_API_PID" ] && kill -0 "$MOCK_API_PID" 2>/dev/null; then
        kill "$MOCK_API_PID" 2>/dev/null || true
        wait "$MOCK_API_PID" 2>/dev/null || true
        echo "  Mock API server stopped."
    fi
    if [ -d "$TEMP_AUTH_DIR" ]; then
        rm -rf "$TEMP_AUTH_DIR"
        echo "  Temp auth dir removed."
    fi
    if [ -d "${TEMP_HOOK_DIR:-}" ]; then
        rm -rf "$TEMP_HOOK_DIR"
        echo "  Temp hook dir removed."
    fi
    echo ""
}
trap cleanup EXIT

# ============================================================
echo -e "${CYAN}============================================${NC}"
echo -e "${CYAN}  auto-reregister Hook — Simulation Test${NC}"
echo -e "${CYAN}============================================${NC}"
echo ""

# --- Prepare temp working copy of hook dir ---
# We copy the hook dir so we can safely swap out the register script
TEMP_HOOK_DIR=$(mktemp -d)
cp -r "$HOOK_DIR/"* "$TEMP_HOOK_DIR/"

# Replace real register script with mock
cp "$TEST_DIR/mock_register.py" "$TEMP_HOOK_DIR/openai_register3.py"
# Empty requirements — mock script has no real dependencies
: > "$TEMP_HOOK_DIR/requirements.txt"

# Remove venv if it exists (force fresh mock setup)
rm -rf "$TEMP_HOOK_DIR/.venv"

# Temp auth directory
TEMP_AUTH_DIR=$(mktemp -d)

# Pre-populate a fake old credential
OLD_CRED_ID="token_old_abcdef.json"
echo '{"email":"old@test.com","access_token":"old-token","type":"codex"}' > "${TEMP_AUTH_DIR}/${OLD_CRED_ID}"

echo -e "${YELLOW}[setup]${NC} Temp hook dir:  $TEMP_HOOK_DIR"
echo -e "${YELLOW}[setup]${NC} Temp auth dir:  $TEMP_AUTH_DIR"
echo -e "${YELLOW}[setup]${NC} Old credential: ${OLD_CRED_ID}"
echo ""

# --- Step A: Start mock API server ---
echo -e "${CYAN}[step A]${NC} Starting mock API server on port ${MOCK_PORT}..."
python3 "$TEST_DIR/mock_api_server.py" "$MOCK_PORT" &
MOCK_API_PID=$!
sleep 1

if kill -0 "$MOCK_API_PID" 2>/dev/null; then
    pass "Mock API server started (PID=$MOCK_API_PID)"
else
    fail "Mock API server failed to start"
    exit 1
fi
echo ""

# --- Step B: Execute run.sh with mock environment ---
echo -e "${CYAN}[step B]${NC} Executing run.sh with simulated environment..."
echo "-----------------------------------------------------------"

# Patch SCRIPT_DIR inside the temp copy so it references itself
# (run.sh uses SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)")
# Since we call it from the temp dir, this works automatically.

export CREDENTIAL_ID="$OLD_CRED_ID"
export MODEL="gpt-4o"
export ROUTE_ID="route-test-001"
export ROUTE_NAME="测试路由"
export TARGET_ID="target-failed-001"
export STATUS_CODE="401"
export ERROR_MESSAGE="Unauthorized: invalid token"
export TRIGGER_REASON="status_code_match"

export PARAM_AUTH_DIR="$TEMP_AUTH_DIR"
export PARAM_API_BASE="http://127.0.0.1:${MOCK_PORT}/v0/management"
export PARAM_API_PASSWORD=""
export PARAM_REGISTER_TYPE="codex"
export PARAM_PROXY=""
export PARAM_SS_DNS=""

RUN_EXIT_CODE=0
bash "$TEMP_HOOK_DIR/run.sh" 2>&1 || RUN_EXIT_CODE=$?

echo "-----------------------------------------------------------"
echo ""

# --- Step C: Verify results ---
echo -e "${CYAN}[step C]${NC} Verifying results..."
echo ""

# C1: run.sh should exit 0
if [ "$RUN_EXIT_CODE" -eq 0 ]; then
    pass "run.sh exited with code 0"
else
    fail "run.sh exited with code $RUN_EXIT_CODE (expected 0)"
fi

# C2: Old credential should be deleted
if [ ! -f "${TEMP_AUTH_DIR}/${OLD_CRED_ID}" ]; then
    pass "Old credential file deleted from auth dir"
else
    fail "Old credential file still exists: ${TEMP_AUTH_DIR}/${OLD_CRED_ID}"
fi

# C3: A new token file should exist in auth dir
NEW_CRED_FILES=$(ls "$TEMP_AUTH_DIR"/token_*.json 2>/dev/null || true)
NEW_CRED_COUNT=$(echo "$NEW_CRED_FILES" | grep -c "token_" || true)

if [ "$NEW_CRED_COUNT" -ge 1 ]; then
    pass "New credential file found in auth dir ($NEW_CRED_COUNT file(s))"
    NEW_CRED_PATH=$(echo "$NEW_CRED_FILES" | head -1)
    NEW_CRED_NAME=$(basename "$NEW_CRED_PATH")
    echo -e "       ${YELLOW}→${NC} $NEW_CRED_NAME"
else
    fail "No new credential file found in auth dir"
    NEW_CRED_NAME=""
fi

# C4: New credential should have correct type
if [ -n "$NEW_CRED_NAME" ] && [ -f "$TEMP_AUTH_DIR/$NEW_CRED_NAME" ]; then
    TOKEN_TYPE=$(python3 -c "import json; print(json.load(open('$TEMP_AUTH_DIR/$NEW_CRED_NAME'))['type'])")
    if [ "$TOKEN_TYPE" = "codex" ]; then
        pass "New credential type = 'codex'"
    else
        fail "New credential type = '$TOKEN_TYPE' (expected 'codex')"
    fi
fi

# C5: Check mock API call log
echo ""
echo -e "${CYAN}[step C.5]${NC} Checking mock API call log..."

API_LOG=$(curl -sf "http://127.0.0.1:${MOCK_PORT}/v0/management/_test/calls" || echo "[]")

# DELETE auth-files call
DELETE_CALLS=$(echo "$API_LOG" | python3 -c "
import json, sys
calls = json.load(sys.stdin)
deletes = [c for c in calls if c['method'] == 'DELETE' and c['endpoint'] == 'auth-files']
print(len(deletes))
")
if [ "$DELETE_CALLS" -ge 1 ]; then
    pass "API DELETE /auth-files called ($DELETE_CALLS time(s))"
else
    fail "API DELETE /auth-files not called"
fi

# POST auth-files call
POST_CALLS=$(echo "$API_LOG" | python3 -c "
import json, sys
calls = json.load(sys.stdin)
posts = [c for c in calls if c['method'] == 'POST' and c['endpoint'] == 'auth-files']
print(len(posts))
")
if [ "$POST_CALLS" -ge 1 ]; then
    pass "API POST /auth-files called ($POST_CALLS time(s))"
else
    fail "API POST /auth-files not called"
fi

# GET pipeline call
GET_PIPELINE=$(echo "$API_LOG" | python3 -c "
import json, sys
calls = json.load(sys.stdin)
gets = [c for c in calls if c['method'] == 'GET' and c['endpoint'] == 'pipeline']
print(len(gets))
")
if [ "$GET_PIPELINE" -ge 1 ]; then
    pass "API GET pipeline called ($GET_PIPELINE time(s))"
else
    fail "API GET pipeline not called"
fi

# PUT pipeline call
PUT_PIPELINE=$(echo "$API_LOG" | python3 -c "
import json, sys
calls = json.load(sys.stdin)
puts = [c for c in calls if c['method'] == 'PUT' and c['endpoint'] == 'pipeline']
print(len(puts))
")
if [ "$PUT_PIPELINE" -ge 1 ]; then
    pass "API PUT pipeline called ($PUT_PIPELINE time(s))"
else
    fail "API PUT pipeline not called"
fi

# C6: Verify pipeline was updated with new credential
if [ "$PUT_PIPELINE" -ge 1 ] && [ -n "$NEW_CRED_NAME" ]; then
    PIPELINE_HAS_NEW=$(echo "$API_LOG" | python3 -c "
import json, sys
calls = json.load(sys.stdin)
puts = [c for c in calls if c['method'] == 'PUT' and c['endpoint'] == 'pipeline']
if puts:
    body = puts[-1].get('body', {})
    layers = body.get('layers', [])
    for layer in layers:
        for t in layer.get('targets', []):
            if t.get('credential_id') == '$NEW_CRED_NAME':
                print('yes')
                sys.exit(0)
print('no')
")
    if [ "$PIPELINE_HAS_NEW" = "yes" ]; then
        pass "Pipeline layer 1 contains new credential ($NEW_CRED_NAME)"
    else
        fail "Pipeline layer 1 does not contain new credential"
    fi
fi

# C7: Verify pipeline still has existing target (not wiped)
PIPELINE_HAS_EXISTING=$(echo "$API_LOG" | python3 -c "
import json, sys
calls = json.load(sys.stdin)
puts = [c for c in calls if c['method'] == 'PUT' and c['endpoint'] == 'pipeline']
if puts:
    body = puts[-1].get('body', {})
    layers = body.get('layers', [])
    for layer in layers:
        for t in layer.get('targets', []):
            if t.get('credential_id') == 'token_existing.json':
                print('yes')
                sys.exit(0)
print('no')
")
if [ "$PIPELINE_HAS_EXISTING" = "yes" ]; then
    pass "Pipeline still contains pre-existing target (token_existing.json)"
else
    fail "Pipeline lost pre-existing target"
fi

# ============================================================
echo ""
echo -e "${CYAN}============================================${NC}"
echo -e "${CYAN}  Test Summary${NC}"
echo -e "${CYAN}============================================${NC}"
echo -e "  ${GREEN}Passed: $PASS_COUNT${NC}"
echo -e "  ${RED}Failed: $FAIL_COUNT${NC}"
echo ""

if [ "$FAIL_COUNT" -eq 0 ]; then
    echo -e "  ${GREEN}ALL TESTS PASSED ✓${NC}"
    exit 0
else
    echo -e "  ${RED}SOME TESTS FAILED ✗${NC}"
    exit 1
fi
