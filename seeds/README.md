# Identity service seeds

## Asset seeds

Run PostgreSQL migrations, SQL seeds, and MinIO asset seeding in one setup
deployment Job, not from each `identity-service` replica:

```sh
make setup
```

The command uses `identity-service/config.yaml`. For another config path, run:

```sh
go run ./identity-service/cmd/setup --config=/path/to/config.yaml
```

The service startup no longer applies migrations or seeds. The reusable
orchestration lives in `pkg/setup`; each service's small `cmd/setup` entry point
passes its schema and embedded migration filesystem. Configure the Job to
complete successfully before starting or rolling out application replicas.
PostgreSQL advisory locks remain enabled as a safeguard if setup Jobs overlap.

Assets are read from `seeds.assets/<level>/...`, for levels listed in
`seeds.levels` in order. The level name is omitted from each MinIO object key;
later levels replace earlier files with the same relative path. SHA-256
metadata avoids uploading unchanged files. Assets absent from the configured
levels are not deleted.

Instances that use different levels but write the same key to the same bucket
still share that object namespace. Use separate `assetsBucket` values when
their assets must remain isolated.
