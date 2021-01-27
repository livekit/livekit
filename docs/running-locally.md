# Running Locally

## Using `docker-compose`

If you have `docker-compose` installed, you should be able to just spin up the services:

```b
$ docker-compose up
```

To speed up the build, you can run a parallel build first (optional):

```b
$ docker-compose build --parallel
$ docker-compose up
```

The services will be available on these ports:

| Service Name     | Port |
| ---------------- | ---- |
| `livekit-server` | 7880 |
| `redis`          | 6379 |

