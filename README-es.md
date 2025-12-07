# LiveKit: servidor SFU WebRTC

LiveKit es un servidor de medios (SFU) para video, audio y datos en tiempo real. Este README resume cómo instalarlo y ejecutarlo en local.

## Requisitos
- Go 1.23+ en `PATH` (para compilar/ejecutar desde fuente)
- Git
- (Opcional) bash/WSL para usar `bootstrap.sh`

## Instalación rápida (binarios)
- **macOS:** `brew install livekit`
- **Linux:** `curl -sSL https://get.livekit.io | bash`
- **Windows:** descarga la última release (`livekit-server.exe`) desde GitHub Releases.

## Ejecutar en modo desarrollo
Usa credenciales por defecto `API Key: devkey` y `API Secret: secret`.

```bash
livekit-server --dev
```

En Windows, si solo tienes el código fuente, puedes correr sin compilar previo:

```bash
go run ./cmd/server --dev
```

El servidor escucha en `ws://localhost:7880`.

## Compilar desde fuente
```bash
git clone https://github.com/livekit/livekit
cd livekit
bash ./bootstrap.sh   # instala dependencias y mage (opcional en Windows)
mage                  # compila livekit-server
```

Sin bash/mage (Windows):
```powershell
go build -o livekit-server.exe ./cmd/server
```

## Configuración
Copia el ejemplo y ajusta:
```bash
cp config-sample.yaml config.yaml
# edita config.yaml según tus necesidades
```
Arranca usando tu archivo:
```bash
livekit-server --config config.yaml
```

## Generar tokens de acceso
Instala el CLI `lk` (https://github.com/livekit/livekit-cli) y genera un token de prueba:
```bash
lk token create --api-key devkey --api-secret secret --join --room mi-sala --identity usuario1 --valid-for 24h
```

## Probar con el demo web
Abre https://example.livekit.io y pega el token generado para conectarte a tu servidor local.

## Despliegue
- La forma más rápida y gestionada: LiveKit Cloud (https://cloud.livekit.io)
- Para auto-hospedado: consulta la guía de despliegue https://docs.livekit.io/deploy/

## Docker (Ubuntu)
Construir la imagen Ubuntu desde este repo (usa `Dockerfile.ubuntu`):

```bash
docker build -f Dockerfile.ubuntu -t livekit-ubuntu .
```

### Ejecutar en local (modo dev)
Exponiendo puertos en localhost:

```bash
docker run --rm -p 7880:7880 -p 7881:7881 -p 7882:7882/udp livekit-ubuntu --dev
```

### Ejecutar con IP/dominio externo
Para que clientes remotos se conecten (necesario si el servidor tiene IP pública o dominio):

1. Edita `config-docker.yaml` y descomenta/configura:
   - `external_ip: "TU_IP_PUBLICA"` (ej: `203.0.113.45`)
   - O `external_hostname: "livekit.tudominio.com"`

2. Ejecuta con tu configuración:
```bash
docker run --rm \
  -p 7880:7880 -p 7881:7881 -p 7882:7882/udp \
  -v "$(pwd)/config-docker.yaml:/config.yaml" \
  livekit-ubuntu --config /config.yaml
```

En Windows PowerShell usa `${PWD}` en vez de `$(pwd)`:
```powershell
docker run --rm `
  -p 7880:7880 -p 7881:7881 -p 7882:7882/udp `
  -v "${PWD}/config-docker.yaml:/config.yaml" `
  livekit-ubuntu --config /config.yaml
```

### Desplegar en Railway
El contenedor detecta automáticamente el entorno Railway y usa el puerto asignado dinámicamente:

1. **Sube la imagen a Docker Hub o GitHub Container Registry:**
```bash
docker tag livekit-ubuntu tuusuario/livekit-ubuntu:latest
docker push tuusuario/livekit-ubuntu:latest
```

2. **En Railway:**
   - Crea un nuevo proyecto desde Docker image
   - Imagen: `tuusuario/livekit-ubuntu:latest`
   - Start Command: `--dev` (o deja vacío si está en ENTRYPOINT)
   - Railway asignará automáticamente un dominio público

3. **Variables de entorno opcionales en Railway:**
   - `PORT` - Railway lo asigna automáticamente
   - Railway expondrá el servicio en su dominio (ej: `tuapp.up.railway.app`)

El servidor detectará Railway y ajustará su configuración automáticamente. Revisa los logs para obtener el token JWT generado.

### Generar token
Con LiveKit CLI en contenedor (sin instalar nada local):

```bash
docker run --rm ghcr.io/livekit/livekit-cli:latest \
  token create \
  --api-key devkey --api-secret secret \
  --join --room mi-sala --identity usuario1 \
  --valid-for 24h
```

El servidor queda accesible en `ws://localhost:7880` (local) o `ws://TU_IP:7880` (remoto) usando claves `devkey/secret`.## Recursos
- Docs generales: https://docs.livekit.io
- SDKs y ejemplos: https://github.com/livekit/livekit-examples
- Comunidad Slack: https://livekit.io/join-slack
