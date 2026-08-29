# MWS IOPS Autoscaler

Watches an NBS-PL2 disk's IOPS demand and raises its provisioned limit within bounds you set. See
`docs/prd-mws-iops-autoscaler.md` for what it does and why, `docs/design.md` for how, and
`docs/configuration.md` for the config file format.

Phase 1 (current): `local` mode, scale-up only. See `docs/prd-mws-iops-autoscaler.md` §6.

## Build

```
go build -o bin/mws-iops-autoscaler ./cmd/mws-iops-autoscaler
```

## Validate a config

```
./bin/mws-iops-autoscaler -config path/to/config.yaml -validate-config
```

Exits 0 and prints a one-line summary if the config is valid; otherwise exits 1 and lists every
violation found, one per line. See `docs/configuration.md` §8 for the full rule set.

## Test

```
go test ./...
```

## Documentation

`docs/README.md` indexes everything, including the PRD/design/ADR hierarchy and its sync rule.
