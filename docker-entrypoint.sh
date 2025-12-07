#!/bin/bash
# Entrypoint script para LiveKit - muestra info de conexión al iniciar

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
    echo "🌐 Servidor WebSocket: ws://localhost:7880"
    echo "   (usa la IP/dominio del host si accedes remotamente)"
    echo ""
    
    # Generar token usando JWT firmado manualmente (Python en contenedor)
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

echo "📡 Puertos expuestos:"
echo "   - 7880 (HTTP/WebSocket)"
echo "   - 7881 (RTC TCP)"
echo "   - 7882 (RTC UDP)"
echo ""
echo "🚀 Iniciando LiveKit Server..."
echo "========================================"
echo ""

# Ejecutar el servidor con los argumentos proporcionados
exec /usr/local/bin/livekit-server "$@"
