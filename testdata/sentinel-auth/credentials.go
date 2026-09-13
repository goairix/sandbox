// Package sentinelauth contains public dummy values for an isolated test fixture.
package sentinelauth

const (
	MasterName       = "auth-fixture-master"
	DataUsername     = "data-user"
	SentinelUsername = "sentinel-user"
	DataPassword     = `data secret "slash\ 值`
	SentinelPassword = `sentinel secret "slash\ 值`
	SentinelAddrs    = "sentinel-0:26379,sentinel-1:26379,sentinel-2:26379"
)
