#!/bin/bash
# auto-reregister hook
#
# 当路由节点失败时自动执行：
# 1. 删除触发失败的凭证 JSON
# 2. 注册一个新的 OpenAI 账号（单线程轮转代理+邮箱源，成功即停）
# 3. 将新凭证 JSON 写入凭证目录
# 4. 通过 API 将新凭证添加到指定 pipeline layer
#
# 系统环境变量（由 HookExecutor 注入）：
#   CREDENTIAL_ID  — 触发此钩子的凭证文件名
#   MODEL          — 触发时使用的模型名
#   ROUTE_ID       — 路由 ID
#   ROUTE_NAME     — 路由名称
#   TARGET_ID      — 目标节点 ID
#   STATUS_CODE    — HTTP 状态码
#   ERROR_MESSAGE  — 错误信息
#
# 自定义参数（由 params.json 定义，以 PARAM_ 前缀注入）：
#   PARAM_CONFIG_JSON_PATH — 外部 config.json 路径（含 proxy_pool 代理池，优先级最高）
#   PARAM_PROXY            — 单个注册代理（config.json 存在时忽略）
#   PARAM_SS_DNS           — SS DNS
#   PARAM_AUTH_DIR         — 凭证目录
#   PARAM_API_BASE         — 管理 API 地址
#   PARAM_API_PASSWORD     — 管理 API 密码
#   PARAM_REGISTER_TYPE    — 账号类型 (codex / chatgpt)
#   PARAM_PIPELINE_MODEL   — 添加到 pipeline 的模型名（留空用触发时的 MODEL）
#   PARAM_PIPELINE_LAYER   — 添加到第几层 pipeline（默认 1）

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "================================================"
echo "[auto-reregister] Hook triggered"
echo "  Time:        $(date -Iseconds)"
echo "  Route:       ${ROUTE_NAME:-N/A} (${ROUTE_ID:-N/A})"
echo "  Credential:  ${CREDENTIAL_ID:-N/A}"
echo "  Model:       ${MODEL:-N/A}"
echo "  Status Code: ${STATUS_CODE:-0}"
echo "  Reason:      ${TRIGGER_REASON:-manual}"
echo "  Manual:      ${MANUAL_TRIGGER:-false}"
echo "================================================"

# --- Resolve parameters ---
AUTH_DIR="${PARAM_AUTH_DIR:-$HOME/.cli-proxy-api}"
AUTH_DIR="${AUTH_DIR/#\~/$HOME}"
API_BASE="${PARAM_API_BASE:-http://127.0.0.1:10101/v0/management}"
API_PASSWORD="${PARAM_API_PASSWORD:-}"
REGISTER_TYPE="${PARAM_REGISTER_TYPE:-codex}"
PROXY="${PARAM_PROXY:-}"
SS_DNS="${PARAM_SS_DNS:-}"
CONFIG_JSON_PATH="${PARAM_CONFIG_JSON_PATH:-}"
CONFIG_JSON_PATH="${CONFIG_JSON_PATH/#\~/$HOME}"
PIPELINE_MODEL="${PARAM_PIPELINE_MODEL:-${MODEL:-unknown}}"
PIPELINE_LAYER="${PARAM_PIPELINE_LAYER:-1}"

# --- Step 1: Delete old credential (non-fatal) ---
AUTH_HEADER=""
if [ -n "$API_PASSWORD" ]; then
    AUTH_HEADER="Authorization: Bearer ${API_PASSWORD}"
fi

if [ -z "${CREDENTIAL_ID:-}" ]; then
    echo "[Step 1] No credential ID provided, skipping delete."
else
    OLD_JSON_PATH="${AUTH_DIR}/${CREDENTIAL_ID}"
    if [ -f "$OLD_JSON_PATH" ]; then
        echo "[Step 1] Deleting old credential: ${OLD_JSON_PATH}"
        rm -f "$OLD_JSON_PATH"
        echo "  Deleted."
    else
        echo "[Step 1] Old credential file not found at ${OLD_JSON_PATH}, skipping file delete."
    fi

    echo "[Step 1] Notifying API to remove credential..."
    ENCODED_CRED=$(python3 -c "import urllib.parse; print(urllib.parse.quote('${CREDENTIAL_ID}', safe=''))" 2>/dev/null || echo "${CREDENTIAL_ID}")
    curl -sf -X DELETE \
        ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
        "${API_BASE}/auth-files?name=${ENCODED_CRED}" \
        && echo "  API delete OK" \
        || echo "  Warning: API delete failed (credential may not exist, continuing)"

    # Remove from pipeline targets (all layers)
    if [ -n "${ROUTE_ID:-}" ]; then
        echo "[Step 1] Removing credential from route ${ROUTE_ID} pipeline..."
        PIPELINE_JSON=$(curl -sf \
            ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
            "${API_BASE}/unified-routing/config/routes/${ROUTE_ID}/pipeline" \
            || echo "")

        if [ -n "$PIPELINE_JSON" ] && [ "$PIPELINE_JSON" != "{}" ]; then
            CLEANED_PIPELINE=$(python3 -c "
import json, sys
pipeline_str = '''${PIPELINE_JSON}'''
try:
    pipeline = json.loads(pipeline_str)
except:
    sys.exit(0)

cred_id = '${CREDENTIAL_ID}'
removed = 0
for layer in pipeline.get('layers', []):
    before = len(layer.get('targets', []))
    layer['targets'] = [t for t in layer.get('targets', []) if t.get('credential-id', t.get('credential_id', '')) != cred_id]
    removed += before - len(layer['targets'])

if removed > 0:
    print(json.dumps(pipeline))
else:
    print('')
" 2>/dev/null || echo "")

            if [ -n "$CLEANED_PIPELINE" ]; then
                curl -sf -X PUT \
                    ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
                    -H "Content-Type: application/json" \
                    -d "$CLEANED_PIPELINE" \
                    "${API_BASE}/unified-routing/config/routes/${ROUTE_ID}/pipeline" \
                    && echo "  Pipeline cleaned: removed targets with credential ${CREDENTIAL_ID}" \
                    || echo "  Warning: Pipeline update failed"
            else
                echo "  Credential not found in pipeline targets, skipping."
            fi
        else
            echo "  No pipeline data found for route, skipping."
        fi
    fi
fi
echo "[Step 1] Done (continuing to registration)."

# --- Step 2: Register new account (single-threaded rotation) ---
echo "[Step 2] Registering new OpenAI account..."

cd "$SCRIPT_DIR"

# Ensure venv
if [ ! -d "${SCRIPT_DIR}/.venv" ]; then
    echo "  Creating venv..."
    python3 -m venv "${SCRIPT_DIR}/.venv"
    "${SCRIPT_DIR}/.venv/bin/pip" install -r "${SCRIPT_DIR}/requirements.txt" -q
fi

NEWEST_TOKEN=""

if [ -n "$CONFIG_JSON_PATH" ] && [ -f "$CONFIG_JSON_PATH" ]; then
    echo "  Using external config for single-threaded rotation: ${CONFIG_JSON_PATH}"

    # Extract proxy_pool and mail_providers from config.json, try each one at a time
    PROXY_COUNT=$(python3 -c "
import json
try:
    c = json.load(open('${CONFIG_JSON_PATH}'))
    pool = c.get('proxy_pool', [])
    print(len(pool))
except:
    print('0')
" 2>/dev/null || echo "0")

    MAIL_PROVIDERS=$(python3 -c "
import json
try:
    c = json.load(open('${CONFIG_JSON_PATH}'))
    providers = c.get('mail_providers', ['dropmail', 'mailtm', 'tempmailio'])
    if not isinstance(providers, list) or not providers:
        providers = ['dropmail', 'mailtm', 'tempmailio']
    print(' '.join(str(p) for p in providers))
except:
    print('dropmail mailtm tempmailio')
" 2>/dev/null || echo "dropmail mailtm tempmailio")

    read -ra MAIL_ARR <<< "$MAIL_PROVIDERS"
    MAIL_COUNT=${#MAIL_ARR[@]}

    # Rotation state: remember where last successful/last run ended
    STATE_FILE="${SCRIPT_DIR}/.rotation_state"
    START_OFFSET=0
    if [ -f "$STATE_FILE" ]; then
        START_OFFSET=$(cat "$STATE_FILE" 2>/dev/null) || true
        START_OFFSET=$((START_OFFSET + 0))  # ensure numeric
    fi

    echo "  Proxy pool: ${PROXY_COUNT} proxies"
    echo "  Mail providers: ${MAIL_PROVIDERS} (${MAIL_COUNT} sources)"
    echo "  Rotation offset: ${START_OFFSET} (continues from last run)"
    echo "  Strategy: try one proxy + one mail source at a time, rotate until success"

    if [ "$PROXY_COUNT" -eq 0 ]; then
        echo "  [WARN] No proxies in config, trying direct with --once..."
        echo '{}' > "${SCRIPT_DIR}/config.json"
        "${SCRIPT_DIR}/.venv/bin/python3" "${SCRIPT_DIR}/openai_register3.py" --once 2>&1 || true
        NEWEST_TOKEN=$(ls -1t "${SCRIPT_DIR}/output"/token_*.json 2>/dev/null | head -1) || true
    else
        MAX_ATTEMPTS=$((PROXY_COUNT * MAIL_COUNT))
        echo "  Max attempts: ${MAX_ATTEMPTS} (${PROXY_COUNT} proxies × ${MAIL_COUNT} mail sources)"

        STEP=0
        while [ -z "$NEWEST_TOKEN" ] && [ "$STEP" -lt "$MAX_ATTEMPTS" ]; do
            GLOBAL_IDX=$((START_OFFSET + STEP))
            PROXY_IDX=$((GLOBAL_IDX % PROXY_COUNT))
            MAIL_IDX=$((GLOBAL_IDX % MAIL_COUNT))
            CURRENT_MAIL="${MAIL_ARR[$MAIL_IDX]}"

            CURRENT_PROXY=$(python3 -c "
import json
c = json.load(open('${CONFIG_JSON_PATH}'))
pool = c.get('proxy_pool', [])
print(pool[${PROXY_IDX}])
" 2>/dev/null) || true

            if [ -z "$CURRENT_PROXY" ]; then
                STEP=$((STEP + 1))
                continue
            fi

            echo ""
            echo "  ── Attempt $((STEP + 1))/${MAX_ATTEMPTS} [proxy $((PROXY_IDX + 1))/${PROXY_COUNT}] ──"
            echo "  Proxy: ${CURRENT_PROXY:0:60}..."
            echo "  Mail:  ${CURRENT_MAIL}"

            python3 -c "
import json
src = json.load(open('${CONFIG_JSON_PATH}'))
cfg = {
    'proxy': src.get('proxy', ''),
    'ss_dns': src.get('ss_dns', ''),
    'proxy_pool': ['${CURRENT_PROXY}'],
    'mail_providers': ['${CURRENT_MAIL}'],
}
if 'dropmail_token' in src:
    cfg['dropmail_token'] = src['dropmail_token']
with open('${SCRIPT_DIR}/config.json', 'w') as f:
    json.dump(cfg, f)
" 2>/dev/null || true

            rm -f "${SCRIPT_DIR}/output"/token_*.json 2>/dev/null || true

            "${SCRIPT_DIR}/.venv/bin/python3" "${SCRIPT_DIR}/openai_register3.py" --once 2>&1 || true

            NEWEST_TOKEN=$(ls -1t "${SCRIPT_DIR}/output"/token_*.json 2>/dev/null | head -1) || true
            if [ -n "$NEWEST_TOKEN" ]; then
                echo "  ✓ Registration succeeded! (attempt $((STEP + 1)), proxy $((PROXY_IDX + 1)))"
                # Save next position for future runs
                echo "$((GLOBAL_IDX + 1))" > "$STATE_FILE"
                break
            else
                echo "  ✗ No token produced, trying next..."
            fi

            STEP=$((STEP + 1))
        done

        # Even if all failed, advance the offset so next trigger starts fresh
        if [ -z "$NEWEST_TOKEN" ]; then
            echo "$((START_OFFSET + MAX_ATTEMPTS))" > "$STATE_FILE"
        fi
    fi

elif [ -n "$PROXY" ]; then
    echo "  Using single proxy: ${PROXY}"
    python3 -c "
import json
cfg = {'proxy': '${PROXY}', 'proxy_pool': ['${PROXY}']}
dns = '${SS_DNS}'
if dns:
    cfg['ss_dns'] = dns
with open('${SCRIPT_DIR}/config.json', 'w') as f:
    json.dump(cfg, f)
"
    "${SCRIPT_DIR}/.venv/bin/python3" "${SCRIPT_DIR}/openai_register3.py" --once 2>&1 || {
        echo "[ERROR] Registration script failed with exit code $?"
        exit 1
    }
    NEWEST_TOKEN=$(ls -1t "${SCRIPT_DIR}/output"/token_*.json 2>/dev/null | head -1) || true
else
    echo "  No proxy configured, using direct connection."
    echo '{}' > "${SCRIPT_DIR}/config.json"
    "${SCRIPT_DIR}/.venv/bin/python3" "${SCRIPT_DIR}/openai_register3.py" --once 2>&1 || {
        echo "[ERROR] Registration script failed with exit code $?"
        exit 1
    }
    NEWEST_TOKEN=$(ls -1t "${SCRIPT_DIR}/output"/token_*.json 2>/dev/null | head -1) || true
fi

# --- Step 3: Find the newest token file ---
echo ""
echo "[Step 3] Looking for generated token file..."

if [ -z "$NEWEST_TOKEN" ]; then
    echo "[ERROR] No token file found in ${SCRIPT_DIR}/output/"
    exit 1
fi

echo "  Found: ${NEWEST_TOKEN}"

python3 -c "
import json
with open('${NEWEST_TOKEN}', 'r') as f:
    data = json.load(f)
data['type'] = '${REGISTER_TYPE}'
with open('${NEWEST_TOKEN}', 'w') as f:
    json.dump(data, f, indent=2)
print('  Type set to: ${REGISTER_TYPE}')
"

# --- Step 4: Copy to auth dir ---
NEW_FILENAME=$(basename "$NEWEST_TOKEN")
NEW_AUTH_PATH="${AUTH_DIR}/${NEW_FILENAME}"

echo "[Step 4] Copying new credential to: ${NEW_AUTH_PATH}"
cp "$NEWEST_TOKEN" "$NEW_AUTH_PATH"

echo "  Uploading via API..."
curl -sf -X POST \
    ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
    -H "Content-Type: application/json" \
    --data-binary @"$NEWEST_TOKEN" \
    "${API_BASE}/auth-files?name=${NEW_FILENAME}" \
    && echo "  Upload OK" \
    || echo "  Warning: API upload failed"

# --- Step 5: Add to route pipeline ---
if [ -z "${ROUTE_ID:-}" ]; then
    echo "[Step 5] No route ID provided, skipping pipeline update."
else
    echo "[Step 5] Adding credential to route ${ROUTE_ID} pipeline layer ${PIPELINE_LAYER}..."
    echo "  Model: ${PIPELINE_MODEL}"

    PIPELINE_JSON=$(curl -sf \
        ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
        "${API_BASE}/unified-routing/config/routes/${ROUTE_ID}/pipeline" \
        || echo "{}")

    UPDATED_PIPELINE=$(python3 -c "
import json, uuid

pipeline_str = '''${PIPELINE_JSON}'''
try:
    pipeline = json.loads(pipeline_str)
except:
    pipeline = {'layers': []}

target_layer = ${PIPELINE_LAYER}
new_target = {
    'id': 'target-' + uuid.uuid4().hex[:8],
    'credential_id': '${NEW_FILENAME}',
    'model': '${PIPELINE_MODEL}',
    'weight': 1,
    'enabled': True
}

if not pipeline.get('layers'):
    pipeline['layers'] = [{'level': target_layer, 'strategy': 'round-robin', 'targets': []}]

layer_found = False
for layer in pipeline['layers']:
    if layer.get('level', 0) == target_layer:
        layer['targets'].append(new_target)
        layer_found = True
        break

if not layer_found:
    pipeline['layers'].append({'level': target_layer, 'strategy': 'round-robin', 'targets': [new_target]})

print(json.dumps(pipeline))
")

    curl -sf -X PUT \
        ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
        -H "Content-Type: application/json" \
        -d "$UPDATED_PIPELINE" \
        "${API_BASE}/unified-routing/config/routes/${ROUTE_ID}/pipeline" \
        && echo "  Pipeline updated OK" \
        || echo "  Warning: Pipeline update failed"
fi

echo ""
echo "================================================"
echo "[auto-reregister] Complete!"
echo "  Old credential removed: ${CREDENTIAL_ID:-none}"
echo "  New credential added:   ${NEW_FILENAME}"
echo "  Route:                  ${ROUTE_NAME:-N/A}"
echo "  Pipeline model:         ${PIPELINE_MODEL}"
echo "  Pipeline layer:         ${PIPELINE_LAYER}"
echo "================================================"
