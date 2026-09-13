// This generator only writes isolated, public dummy-credential fixture configs.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/goairix/sandbox/internal/redisbootstrap"
	fixture "github.com/goairix/sandbox/testdata/sentinel-auth"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "one private fixture output directory is required")
		os.Exit(1)
	}
	if err := generate(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "cannot generate isolated fixture configuration")
		os.Exit(1)
	}
}

func generate(directory string) error {
	dataACL, err := redisbootstrap.QuoteConfigValue(">" + fixture.DataPassword)
	if err != nil {
		return err
	}
	dataPass, err := redisbootstrap.QuoteConfigValue(fixture.DataPassword)
	if err != nil {
		return err
	}
	sentinelACL, err := redisbootstrap.QuoteConfigValue(">" + fixture.SentinelPassword)
	if err != nil {
		return err
	}
	sentinelPass, err := redisbootstrap.QuoteConfigValue(fixture.SentinelPassword)
	if err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		redisConfig := fmt.Sprintf(`bind 0.0.0.0
protected-mode yes
port 6379
dir /data
appendonly yes
appendfsync everysec
user default off
user %s on %s allcommands allkeys allchannels
masteruser %s
masterauth %s
min-replicas-to-write 1
min-replicas-max-lag 5
replica-announce-ip redis-%d
replica-announce-port 6379
`, fixture.DataUsername, dataACL, fixture.DataUsername, dataPass, i)
		if i != 0 {
			redisConfig += "replicaof redis-0 6379\n"
		}
		sentinelConfig := fmt.Sprintf(`bind 0.0.0.0
protected-mode yes
port 26379
dir /data
user default off
user %s on %s allcommands allkeys allchannels
sentinel sentinel-user %s
sentinel sentinel-pass %s
sentinel resolve-hostnames yes
sentinel announce-hostnames yes
sentinel announce-ip sentinel-%d
sentinel announce-port 26379
sentinel monitor %s redis-0 6379 2
sentinel auth-user %s %s
sentinel auth-pass %s %s
sentinel down-after-milliseconds %s 10000
sentinel failover-timeout %s 30000
sentinel parallel-syncs %s 1
`, fixture.SentinelUsername, sentinelACL, fixture.SentinelUsername, sentinelPass,
			i, fixture.MasterName, fixture.MasterName, fixture.DataUsername,
			fixture.MasterName, dataPass, fixture.MasterName, fixture.MasterName, fixture.MasterName)
		for _, config := range []struct{ name, contents string }{
			{fmt.Sprintf("redis-%d.conf", i), redisConfig},
			{fmt.Sprintf("sentinel-%d.conf", i), sentinelConfig},
		} {
			if err := os.WriteFile(filepath.Join(directory, config.name), []byte(config.contents), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}
