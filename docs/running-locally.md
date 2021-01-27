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