#!/bin/bash
set -e

# ============================================
# LiveKit Cloud Deployment Entrypoint
# Auto-configures for Railway, Render, Fly.io, Heroku, AWS, GCP, Azure
# ============================================

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "========================================"
echo "  🚀 LiveKit Server - Cloud Deployment"
echo "========================================"
echo ""

# Detect cloud platform
CLOUD_PLATFORM="Unknown"
PUBLIC_URL=""

if [ -n "$RAILWAY_ENVIRONMENT" ]; then
    CLOUD_PLATFORM="Railway"
    PUBLIC_URL="${RAILWAY_PUBLIC_DOMAIN:-}"
elif [ -n "$RENDER_EXTERNAL_HOSTNAME" ]; then
    CLOUD_PLATFORM="Render"
    PUBLIC_URL="$RENDER_EXTERNAL_HOSTNAME"
elif [ -n "$FLY_APP_NAME" ]; then
    CLOUD_PLATFORM="Fly.io"
    PUBLIC_URL="${FLY_APP_NAME}.fly.dev"
elif [ -n "$DYNO" ]; then
    CLOUD_PLATFORM="Heroku"
    PUBLIC_URL="${HEROKU_APP_NAME}.herokuapp.com"
elif [ -n "$AWS_EXECUTION_ENV" ]; then
    CLOUD_PLATFORM="AWS"
elif [ -n "$K_SERVICE" ]; then
    CLOUD_PLATFORM="Google Cloud Run"
elif [ -n "$WEBSITE_SITE_NAME" ]; then
    CLOUD_PLATFORM="Azure"
else
    CLOUD_PLATFORM="Generic/Local"
fi

# Use custom domain if provided
if [ -n "$LIVEKIT_DOMAIN" ]; then
    PUBLIC_URL="$LIVEKIT_DOMAIN"
elif [ -n "$PUBLIC_URL_OVERRIDE" ]; then
    PUBLIC_URL="$PUBLIC_URL_OVERRIDE"
fi

echo -e "${BLUE}☁️  Platform:${NC} $CLOUD_PLATFORM"
echo -e "${BLUE}🌐 Public URL:${NC} ${PUBLIC_URL:-'Not detected (using IP)'}"
echo ""

# Detect public IP if not set
if [ -z "$LIVEKIT_NODE_IP" ]; then
    echo -e "${YELLOW}🔍 Detecting public IP...${NC}"
    PUBLIC_IP=$(curl -s https://api.ipify.org 2>/dev/null || curl -s https://ifconfig.me 2>/dev/null || curl -s https://icanhazip.com 2>/dev/null || echo "unknown")
    export LIVEKIT_NODE_IP="$PUBLIC_IP"
    echo -e "${GREEN}✓ Public IP detected:${NC} $PUBLIC_IP"
else
    PUBLIC_IP="$LIVEKIT_NODE_IP"
    echo -e "${GREEN}✓ Using configured IP:${NC} $PUBLIC_IP"
fi
echo ""

# Configure port (cloud platforms set PORT dynamically)
HTTP_PORT="${PORT:-7880}"
RTC_TCP_PORT="${LIVEKIT_RTC_TCP_PORT:-7881}"
RTC_UDP_PORT="${LIVEKIT_RTC_UDP_PORT:-7882}"
BIND_ADDRESS="${LIVEKIT_BIND:-0.0.0.0}"

echo -e "${GREEN}📡 Configuration:${NC}"
echo "   HTTP Port: $HTTP_PORT"
echo "   RTC TCP Port: $RTC_TCP_PORT"
echo "   RTC UDP Port: $RTC_UDP_PORT"
echo "   Bind Address: $BIND_ADDRESS"
echo "   Public IP: $PUBLIC_IP"
echo ""

# API credentials
API_KEY="${LIVEKIT_API_KEY:-APIQmrHaqpJjwXs}"
API_SECRET="${LIVEKIT_API_SECRET:-2j4uCefeStZIjOAoYoVBSSAexGZhrm0aWk7N7mpAYaOA}"

echo -e "${YELLOW}🔑 API Credentials:${NC}"
echo "   API Key: $API_KEY"
echo "   API Secret: ${API_SECRET:0:10}..."
echo ""

# Generate JWT token for testing
echo -e "${GREEN}🎟️  Generating Access Token...${NC}"
HEADER='{"alg":"HS256","typ":"JWT"}'
CURRENT_TIME=$(date +%s)
EXPIRY_TIME=4102444800  # Year 2099
PAYLOAD="{\"exp\":$EXPIRY_TIME,\"identity\":\"user1\",\"iss\":\"$API_KEY\",\"name\":\"user1\",\"nbf\":$CURRENT_TIME,\"sub\":\"user1\",\"video\":{\"room\":\"test-room\",\"roomJoin\":true}}"

HEADER_B64=$(echo -n "$HEADER" | base64 -w 0 2>/dev/null || echo -n "$HEADER" | base64 | tr -d '\n')
HEADER_B64=$(echo -n "$HEADER_B64" | tr '+/' '-_' | tr -d '=')

PAYLOAD_B64=$(echo -n "$PAYLOAD" | base64 -w 0 2>/dev/null || echo -n "$PAYLOAD" | base64 | tr -d '\n')
PAYLOAD_B64=$(echo -n "$PAYLOAD_B64" | tr '+/' '-_' | tr -d '=')

SIGNATURE_RAW=$(echo -n "${HEADER_B64}.${PAYLOAD_B64}" | openssl dgst -sha256 -hmac "$API_SECRET" -binary | base64 -w 0 2>/dev/null || echo -n "${HEADER_B64}.${PAYLOAD_B64}" | openssl dgst -sha256 -hmac "$API_SECRET" -binary | base64 | tr -d '\n')
SIGNATURE=$(echo -n "$SIGNATURE_RAW" | tr '+/' '-_' | tr -d '=')

TOKEN="${HEADER_B64}.${PAYLOAD_B64}.${SIGNATURE}"

echo ""
echo -e "${GREEN}✅ Access Token (valid until 2099):${NC}"
echo "   $TOKEN"
echo ""
echo -e "${BLUE}📋 Token Details:${NC}"
echo "   Room: test-room"
echo "   Identity: user1"
echo "   Permissions: Join room (roomJoin)"
echo ""

# Connection instructions
if [ -n "$PUBLIC_URL" ]; then
    PROTOCOL="wss"
    [ "$CLOUD_PLATFORM" = "Generic/Local" ] && PROTOCOL="ws"
    echo -e "${GREEN}🔗 WebSocket URL:${NC}"
    echo "   ${PROTOCOL}://${PUBLIC_URL}"
elif [ -n "$PUBLIC_IP" ] && [ "$PUBLIC_IP" != "unknown" ]; then
    echo -e "${GREEN}🔗 WebSocket URL (IP):${NC}"
    echo "   ws://${PUBLIC_IP}:${HTTP_PORT}"
else
    echo -e "${YELLOW}⚠️  No public URL/IP detected. Configure LIVEKIT_DOMAIN or PUBLIC_URL_OVERRIDE${NC}"
fi

echo ""
echo "========================================"
echo -e "${GREEN}🚀 Starting LiveKit Server...${NC}"
echo "========================================"
echo ""

# Build command arguments
ARGS=("--port" "$HTTP_PORT" "--bind" "$BIND_ADDRESS")

# Add API keys if not in dev mode
if [[ "$*" != *"--dev"* ]]; then
    ARGS+=("--keys" "${API_KEY}: ${API_SECRET}")
fi

# External IP configuration
if [ -n "$LIVEKIT_NODE_IP" ]; then
    # Use specific node IP if provided
    echo "📍 Using configured node IP: $LIVEKIT_NODE_IP"
    ARGS+=("--node-ip" "$LIVEKIT_NODE_IP")
elif [ "$LIVEKIT_USE_EXTERNAL_IP" = "true" ]; then
    # Let LiveKit auto-detect via STUN
    echo "🔍 Auto-detecting external IP via STUN..."
fi

# Execute LiveKit server
exec /usr/local/bin/livekit-server "$@" "${ARGS[@]}"
