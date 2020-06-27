package cache

import (
	"context"
	b64 "encoding/base64"
	"fmt"
	"strings"
	"time"

	goredis "github.com/go-redis/redis/v8"
	bes "github.com/iegomez/mosquitto-go-auth/backends"
	goCache "github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// redisCache stores necessary values for Redis cache
type redisStore struct {
	authExpiration int64
	aclExpiration  int64
	client         bes.RedisClient
}

type goStore struct {
	authExpiration int64
	aclExpiration  int64
	client         *goCache.Cache
}

const (
	defaultExpiration = 30
)

type Store interface {
	SetAuthRecord(ctx context.Context, username, password, granted string) error
	CheckAuthRecord(ctx context.Context, username, password string) (bool, bool)
	SetACLRecord(ctx context.Context, username, topic, clientid string, acc int, granted string) error
	CheckACLRecord(ctx context.Context, username, topic, clientid string, acc int) (bool, bool)
	Connect(ctx context.Context, reset bool) bool
	Close()
}

// NewGoStore initializes a cache using go-cache as the store.
func NewGoStore(authExpiration, aclExpiration int64) *goStore {
	// TODO: support hydrating the cache to retain previous values.

	return &goStore{
		authExpiration: authExpiration,
		aclExpiration:  aclExpiration,
		client:         goCache.New(time.Second*defaultExpiration, time.Second*(defaultExpiration*2)),
	}
}

// NewSingleRedisStore initializes a cache using a single Redis instance as the store.
func NewSingleRedisStore(host, port, password string, db int, authExpiration, aclExpiration int64) *redisStore {
	addr := fmt.Sprintf("%s:%s", host, port)
	redisClient := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: password, // no password set
		DB:       db,       // use default db
	})
	//If cache is on, try to start redis.
	return &redisStore{
		authExpiration: authExpiration,
		aclExpiration:  aclExpiration,
		client:         bes.SingleRedisClient{redisClient},
	}
}

// NewSingleRedisStore initializes a cache using a Redis Cluster as the store.
func NewRedisClusterStore(password string, addresses []string, authExpiration, aclExpiration int64) *redisStore {
	clusterClient := goredis.NewClusterClient(
		&goredis.ClusterOptions{
			Addrs:    addresses,
			Password: password,
		})

	return &redisStore{
		authExpiration: authExpiration,
		aclExpiration:  aclExpiration,
		client:         clusterClient,
	}
}

func toAuthRecord(username, password string) string {
	return b64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("auth-%s-%s", username, password)))
}

func toACLRecord(username, topic, clientid string, acc int) string {
	return b64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("acl-%s-%s-%s-%d", username, topic, clientid, acc)))
}

// Checks if an error was caused by a moved record in a Redis Cluster.
func isMovedError(err error) bool {
	s := err.Error()
	if strings.HasPrefix(s, "MOVED ") || strings.HasPrefix(s, "ASK ") {
		return true
	}

	return false
}

// Connect flushes the cache if reset is set.
func (s *goStore) Connect(ctx context.Context, reset bool) bool {
	log.Infoln("started go-cache")
	if reset {
		s.client.Flush()
		log.Infoln("flushed go-cache")
	}
	return true
}

// Connect pings Redis and flushes the cache if reset is set.
func (s *redisStore) Connect(ctx context.Context, reset bool) bool {
	_, err := s.client.Ping(ctx).Result()
	if err != nil {
		log.Errorf("couldn't start redis. error: %s", err)
		return false
	} else {
		log.Infoln("started redis cachet")
		//Check if cache must be reset
		if reset {
			s.client.FlushDB(ctx)
			log.Infoln("flushed redis cache")
		}
	}
	return true
}

func (s *goStore) Close() {
	//TODO: support serializing cache for re hydration.
}

func (s *redisStore) Close() {
	s.client.Close()
}

// CheckAuthRecord checks if the username/password pair is present in the cache. Return if it's present and, if so, if it was granted privileges
func (s *goStore) CheckAuthRecord(ctx context.Context, username, password string) (bool, bool) {
	record := toAuthRecord(username, password)
	return s.checkRecord(ctx, record, s.authExpiration)
}

//CheckAclCache checks if the username/topic/clientid/acc mix is present in the cache. Return if it's present and, if so, if it was granted privileges.
func (s *goStore) CheckACLRecord(ctx context.Context, username, topic, clientid string, acc int) (bool, bool) {
	record := toACLRecord(username, topic, clientid, acc)
	return s.checkRecord(ctx, record, s.aclExpiration)
}

func (s *goStore) checkRecord(ctx context.Context, record string, expirationTime int64) (bool, bool) {
	granted := false
	v, present := s.client.Get(record)

	if present {
		value, ok := v.(string)
		if ok && value == "true" {
			granted = true
		}

		s.client.Set(record, value, time.Duration(expirationTime))
	}
	return present, granted
}

// CheckAuthRecord checks if the username/password pair is present in the cache. Return if it's present and, if so, if it was granted privileges
func (s *redisStore) CheckAuthRecord(ctx context.Context, username, password string) (bool, bool) {
	record := toAuthRecord(username, password)
	return s.checkRecord(ctx, record, s.authExpiration)
}

//CheckAclCache checks if the username/topic/clientid/acc mix is present in the cache. Return if it's present and, if so, if it was granted privileges.
func (s *redisStore) CheckACLRecord(ctx context.Context, username, topic, clientid string, acc int) (bool, bool) {
	record := toACLRecord(username, topic, clientid, acc)
	return s.checkRecord(ctx, record, s.aclExpiration)
}

func (s *redisStore) checkRecord(ctx context.Context, record string, expirationTime int64) (bool, bool) {

	present, granted, err := s.getAndRefresh(ctx, record, expirationTime)
	if err == nil {
		return present, granted
	}

	if isMovedError(err) {
		err = s.client.ReloadState(ctx)
		// This should not happen, ever!
		if err == bes.SingleClientError {
			return false, false
		}

		//Retry once.
		present, granted, err = s.getAndRefresh(ctx, record, expirationTime)
	}

	if err != nil {
		log.Debugf("set cache error: %s", err)
	}

	return present, granted
}

func (s *redisStore) getAndRefresh(ctx context.Context, record string, expirationTime int64) (bool, bool, error) {
	val, err := s.client.Get(ctx, record).Result()
	if err != nil {
		return false, false, err
	}

	//refresh expiration
	_, err = s.client.Expire(ctx, record, time.Duration(expirationTime)*time.Second).Result()
	if err != nil {
		return false, false, err
	}

	if val == "true" {
		return true, true, nil
	}

	return true, false, nil
}

// SetAuthRecord sets a pair, granted option and expiration time.
func (s *goStore) SetAuthRecord(ctx context.Context, username, password string, granted string) error {
	record := toAuthRecord(username, password)
	s.client.Set(record, granted, time.Duration(s.authExpiration))

	return nil
}

//SetAclCache sets a mix, granted option and expiration time.
func (s *goStore) SetACLRecord(ctx context.Context, username, topic, clientid string, acc int, granted string) error {
	record := toACLRecord(username, topic, clientid, acc)
	s.client.Set(record, granted, time.Duration(s.authExpiration))

	return nil
}

// SetAuthRecord sets a pair, granted option and expiration time.
func (s *redisStore) SetAuthRecord(ctx context.Context, username, password string, granted string) error {
	record := toAuthRecord(username, password)
	return s.setRecord(ctx, record, granted, s.authExpiration)
}

//SetAclCache sets a mix, granted option and expiration time.
func (s *redisStore) SetACLRecord(ctx context.Context, username, topic, clientid string, acc int, granted string) error {
	record := toACLRecord(username, topic, clientid, acc)
	return s.setRecord(ctx, record, granted, s.authExpiration)
}

func (s *redisStore) setRecord(ctx context.Context, record, granted string, expirationTime int64) error {
	err := s.set(ctx, record, granted, expirationTime)

	if err == nil {
		return nil
	}

	// If record was moved, reload and retry.
	if isMovedError(err) {
		err = s.client.ReloadState(ctx)
		if err != nil {
			return err
		}

		//Retry once.
		err = s.set(ctx, record, granted, expirationTime)
	}

	return err
}

func (s *redisStore) set(ctx context.Context, record string, granted string, expirationTime int64) error {
	return s.client.Set(ctx, record, granted, time.Duration(expirationTime)*time.Second).Err()
}
