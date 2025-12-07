#!/bin/bash
# Entrypoint script para LiveKit - muestra info de conexión al iniciar

# Detectar si estamos en Railway u otro servicio cloud (tienen PORT definido)
HTTP_PORT=${PORT:-7880}
RTC_TCP_PORT=${RTC_TCP_PORT:-7881}
RTC_UDP_START=${RTC_UDP_START:-7882}

echo "========================================"
echo "  LiveKit Server - Información de Conexión"
echo "========================================"
echo ""

# Detectar modo de operación
if [[ "$*" == *"--dev"* ]]; then
    echo "🔧 Modo: DESARROLLO"
    echo "📍 API Key: devkey"
    echo "🔑 API Secret: secret"
    echo ""
    
    # Detectar entorno
    if [ -n "$RAILWAY_ENVIRONMENT" ] || [ -n "$PORT" ]; then
        echo "☁️  Entorno: Railway/Cloud detectado"
        echo "🌐 Servidor WebSocket: Puerto $HTTP_PORT (asignado dinámicamente)"
        echo "   URL pública: usar el dominio proporcionado por Railway"
    else
        echo "🌐 Servidor WebSocket: ws://localhost:$HTTP_PORT"
        echo "   (usa la IP/dominio del host si accedes remotamente)"
    fi
    echo ""
    
    # Generar token usando JWT firmado manualmente
    echo "🎟️  Generando token de acceso (válido indefinidamente)..."
    
    # Crear token JWT usando openssl (ya disponible en Ubuntu)
    HEADER='{"alg":"HS256","typ":"JWT"}'
    # Token sin expiración (exp muy lejano: año 2099)
    PAYLOAD="{\"exp\":4102444800,\"identity\":\"user1\",\"iss\":\"devkey\",\"name\":\"user1\",\"nbf\":$(date +%s),\"sub\":\"user1\",\"video\":{\"room\":\"test-room\",\"roomJoin\":true}}"
    
    HEADER_B64=$(echo -n "$HEADER" | base64 | tr -d '=' | tr '/+' '_-' | tr -d '\n')
    PAYLOAD_B64=$(echo -n "$PAYLOAD" | base64 | tr -d '=' | tr '/+' '_-' | tr -d '\n')
    
    SIGNATURE=$(echo -n "${HEADER_B64}.${PAYLOAD_B64}" | openssl dgst -sha256 -hmac "secret" -binary | base64 | tr -d '=' | tr '/+' '_-' | tr -d '\n')
    
    TOKEN="${HEADER_B64}.${PAYLOAD_B64}.${SIGNATURE}"
    
    echo ""
    echo "✅ Token JWT (válido hasta 2099):"
    echo "   $TOKEN"
    echo ""
    echo "📋 Detalles del token:"
    echo "   - Sala: test-room"
    echo "   - Identidad: user1"
    echo "   - Permisos: Unirse a sala (roomJoin)"
    echo ""
else
    echo "🔧 Modo: PRODUCCIÓN/CONFIGURACIÓN PERSONALIZADA"
    echo "📝 Revisa tu archivo de configuración para credenciales"
    echo ""
fi

echo "📡 Puertos configurados:"
echo "   - $HTTP_PORT (HTTP/WebSocket)"
echo "   - $RTC_TCP_PORT (RTC TCP)"
echo "   - $RTC_UDP_START (RTC UDP)"
echo ""
echo "🚀 Iniciando LiveKit Server..."
echo "========================================"
echo ""

# Si estamos en modo dev y hay PORT definido (Railway/Cloud), agregar --port
if [[ "$*" == *"--dev"* ]] && [ -n "$PORT" ]; then
    exec /usr/local/bin/livekit-server --dev --port "$HTTP_PORT"
else
    # Ejecutar el servidor con los argumentos proporcionados
    exec /usr/local/bin/livekit-server "$@"
fi
