package main

import (
	"errors"
	"os"

	"github.com/redis/go-redis/v9"
)

// redisOptions is the one place the choice between the two ways to reach Redis is
// made. A URL (NEXUS_REDIS_URL: redis:// or rediss:// for TLS, with an optional
// user and password) wins when set; otherwise it is the plain address
// (NEXUS_REDIS_ADDR), which can carry neither a password nor TLS.
//
// Redis carries the job queue, the session locks, the cost counters and the live
// content previews, which are plaintext, so a deployment has to be able to
// authenticate to it and encrypt the connection.
//
// The URL carries a password, so a malformed one is reported without the URL or
// the parser's own message, which quotes it, and so does not put the credential in
// a log.
func redisOptions(url, addr string) (*redis.Options, error) {
	if url == "" {
		return &redis.Options{Addr: addr}, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, errors.New("NEXUS_REDIS_URL is not a valid redis:// or rediss:// URL (redis://[user:password@]host:port/db, rediss:// for TLS)")
	}
	return opts, nil
}

// newRedisClient builds the Redis client from the environment: NEXUS_REDIS_URL if
// set, else NEXUS_REDIS_ADDR (defaulting to the local compose Redis, for the
// operator commands that have always done so).
func newRedisClient() (*redis.Client, error) {
	return newRedisClientFrom(os.Getenv("NEXUS_REDIS_URL"), envOr("NEXUS_REDIS_ADDR", defaultRedisAddr))
}

// newRedisClientFrom is newRedisClient over values already read, as serve reads them
// once into serverConfig.
func newRedisClientFrom(url, addr string) (*redis.Client, error) {
	opts, err := redisOptions(url, addr)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(opts), nil
}
