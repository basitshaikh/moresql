package moresql

import (
	"database/sql"
	"expvar"
	"fmt"

	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/jmoiron/sqlx"
	"github.com/orcaman/concurrent-map"
	"github.com/paulbellamy/ratecounter"
	"github.com/rwynn/gtm"
	"github.com/thejerf/suture"

	mgo "gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

// Tailer is the core struct for performing
// Mongo->Pg streaming.
type Tailer struct {
	config     Config
	pg         *sqlx.DB
	session    *mgo.Session
	env        Env
	counters   counters
	stop       chan bool
	fan        map[string]gtm.OpChan
	checkpoint *cmap.ConcurrentMap
}

// Stop is the func necessary to terminate action
// when using Suture library
func (t *Tailer) Stop() {
	fmt.Println("Stopping service")
	t.stop <- true
}

func (t *Tailer) startOverflowConsumers(c <-chan *gtm.Op) {
	for i := 1; i <= workerCountOverflow; i++ {
		go t.consumer(c, nil)
	}
}

type EpochTimestamp int64

func BuildOptionAfterFromTimestamp(timestamp EpochTimestamp, replayDuration time.Duration) (func(*mgo.Session, *gtm.Options) bson.MongoTimestamp, error) {
	if timestamp != EpochTimestamp(0) && int64(timestamp) < time.Now().Unix() {
		// We have a starting oplog entry
		f := func() time.Time { return time.Unix(int64(timestamp), 0) }
		return OpTimestampWrapper(f, time.Duration(0)), nil
	} else if replayDuration != time.Duration(0) {
		return OpTimestampWrapper(bson.Now, replayDuration), nil
	} else {
		return OpTimestampWrapper(bson.Now, time.Duration(0)), nil
	}
	return nil, fmt.Errorf("Unable to calculate tailing start time")
}

func (t *Tailer) NewOptions(timestamp EpochTimestamp, replayDuration time.Duration) (*gtm.Options, error) {
	options := gtm.DefaultOptions()
	after, err := BuildOptionAfterFromTimestamp(timestamp, replayDuration)
	if err != nil {
		return nil, err
	}
	epoch, _ := gtm.ParseTimestamp(after(nil, nil))
	log.Infof("Starting from epoch: %+v", epoch)
	options.After = after
	options.BufferSize = 500
	options.BufferDuration = time.Duration(500 * time.Millisecond)
	options.Ordering = gtm.Document
	return options, nil
}

func (t *Tailer) NewFan() map[string]gtm.OpChan {
	fan := make(map[string]gtm.OpChan)
	// Register Channels
	for dbName, db := range t.config {
		for collectionName := range db.Collections {
			fan[createFanKey(dbName, collectionName)] = make(gtm.OpChan, 1000)
		}
	}
	return fan
}

func (t *Tailer) startDedicatedConsumers(fan map[string]gtm.OpChan, overflow gtm.OpChan) {
	// Reserved workers for individual channels
	for k, c := range fan {
		var workers [workerCount]int
		log.WithFields(log.Fields{
			"count":      workerCount,
			"collection": k,
		}).Debug("Starting worker(s)")
		for _ = range workers {
			go t.consumer(c, overflow)
		}
	}
}

type MoresqlMetadata struct {
	AppName     string    `db:"app_name"`
	LastEpoch   int64     `db:"last_epoch"`
	ProcessedAt time.Time `db:"processed_at"`
}

func NewTailer(config Config, pg *sqlx.DB, session *mgo.Session, env Env) *Tailer {
	checkpoint := cmap.New()
	return &Tailer{config: config, pg: pg, session: session, env: env, stop: make(chan bool), counters: buildCounters(), checkpoint: &checkpoint}
}

func FetchMetadata(checkpoint bool, pg *sqlx.DB, appName string) MoresqlMetadata {
	metadata := MoresqlMetadata{}
	if checkpoint {
		q := Queries{}
		err := pg.Get(&metadata, q.GetMetadata(), appName)
		// No rows means this is first time with table
		if err != nil && err != sql.ErrNoRows {
			log.Errorf("Error while reading moresql_metadata table %+v", err)
			c := Commands{}
			c.CreateTableSQL()
		}

	} else {
		metadata.LastEpoch = 0
	}
	return metadata
}

func (t *Tailer) Read() {
	metadata := FetchMetadata(t.env.checkpoint, t.pg, t.env.appName)
	options, err := t.NewOptions(EpochTimestamp(metadata.LastEpoch), t.env.replayDuration)
	if err != nil {
		log.Fatal(err.Error())
	}
	ops, errs := gtm.Tail(t.session, options)
	log.Info("Tailing mongo oplog")
	go func() {
		for {
			select {
			case <-t.stop:
				return
			case err := <-errs:
				log.Errorln(err)
			case op := <-ops:
				t.counters.read.Incr(1)
				log.WithFields(log.Fields{
					"operation":  op.Operation,
					"collection": op.GetCollection(),
					"id":         op.Id,
				}).Debug("Received operation")
				// Check if we're watching for the collection
				db := op.GetDatabase()
				coll := op.GetCollection()
				key := createFanKey(db, coll)
				if c := t.fan[key]; c != nil {
					collection := t.config[db].Collections[coll]
					o := Statement{collection}
					c <- EnsureOpHasAllFields(op, o.mongoFields())
				} else {
					t.counters.skipped.Incr(1)
					log.Debug("Missing channel for this collection")
				}
				for k, v := range t.fan {
					if len(v) > 0 {
						log.Debugf("Channel %s has %d", k, len(v))
					}
				}
			}
		}
	}()
}

func (t *Tailer) Write() {
	t.fan = t.NewFan()
	log.WithField("struct", t.fan).Debug("Fan")
	overflow := make(gtm.OpChan)
	t.startDedicatedConsumers(t.fan, overflow)
	t.startOverflowConsumers(overflow)
}

func (t *Tailer) Report() {
	c := time.Tick(time.Duration(reportFrequency) * time.Second)
	go func() {
		for {
			select {
			case <-c:
				t.ReportCounters()
			}
		}
	}()

}

func (t *Tailer) SaveCheckpoint(m MoresqlMetadata) error {
	q := Queries{}
	result, err := t.pg.NamedExec(q.SaveMetadata(), m)
	if err != nil {
		log.Errorf("Unable to save into moresql_metadata: %+v, %+v", result, err.Error())
	}
	return err
}

func (t *Tailer) Checkpoints() {
	go func() {
		timer := time.Tick(checkpointFrequency)
		for {
			select {
			case _ = <-timer:
				latest, ok := t.checkpoint.Get("latest")
				if ok && latest != nil {
					t.SaveCheckpoint(latest.(MoresqlMetadata))
					log.Debug("Saved checkpointing %+v", latest.(MoresqlMetadata))
				}
			}
		}
	}()
}

// Serve is the func necessary to start action
// when using Suture library
func (t *Tailer) Serve() {
	t.Write()
	t.Read()
	t.Report()
	if t.env.checkpoint {
		t.Checkpoints()
	}
	<-t.stop
}

type counters struct {
	insert  *ratecounter.RateCounter
	update  *ratecounter.RateCounter
	delete  *ratecounter.RateCounter
	read    *ratecounter.RateCounter
	skipped *ratecounter.RateCounter
}

func (c *counters) All() map[string]*ratecounter.RateCounter {
	cx := make(map[string]*ratecounter.RateCounter)
	cx["insert"] = c.insert
	cx["update"] = c.update
	cx["delete"] = c.delete
	cx["read"] = c.read
	cx["skipped"] = c.skipped
	return cx
}

func buildCounters() (c counters) {
	c = counters{
		ratecounter.NewRateCounter(1 * time.Minute),
		ratecounter.NewRateCounter(1 * time.Minute),
		ratecounter.NewRateCounter(1 * time.Minute),
		ratecounter.NewRateCounter(1 * time.Minute),
		ratecounter.NewRateCounter(1 * time.Minute),
	}
	expvar.Publish("insert/min", c.insert)
	expvar.Publish("update/min", c.update)
	expvar.Publish("delete/min", c.delete)
	expvar.Publish("ops/min", c.read)
	expvar.Publish("skipped/min", c.skipped)
	return
}

func (t *Tailer) ReportCounters() {
	for i, counter := range t.counters.All() {
		log.Infof("Rate of %s per min: %d", i, counter.Rate())
	}
}

func (t *Tailer) MsLag(epoch int32, nowFunc func() time.Time) int64 {
	// TODO: use time.Duration instead of this malarky
	ts := time.Unix(int64(epoch), 0)
	d := nowFunc().Sub(ts)
	nanoToMillisecond := func(t time.Duration) int64 { return t.Nanoseconds() / 1e6 }
	return nanoToMillisecond(d)
}

func (t *Tailer) consumer(in <-chan *gtm.Op, overflow chan<- *gtm.Op) {
	var workerType string
	if overflow != nil {
		workerType = "Dedicated"
	} else {
		workerType = "Generic"
	}
	for {
		if overflow != nil && len(in) > workerCount {
			// Siphon off overflow
			select {
			case op := <-in:
				overflow <- op
			}
			continue
		}
		select {
		case op := <-in:
			t.processOp(op, workerType)
			if t.env.checkpoint {
				t.checkpoint.Set("latest", t.OpToMoresqlMetadata(op))
			}
		}
	}
}

func (t *Tailer) OpToMoresqlMetadata(op *gtm.Op) MoresqlMetadata {
	ts, _ := gtm.ParseTimestamp(op.Timestamp)
	return MoresqlMetadata{AppName: t.env.appName, ProcessedAt: time.Now(), LastEpoch: int64(ts)}
}

func (t *Tailer) processOp(op *gtm.Op, workerType string) {
	collectionName := op.GetCollection()
	db := op.GetDatabase()
	st := FullSyncer{Config: t.config}
	o, c := st.statementFromDbCollection(db, collectionName)
	ts1, ts2 := gtm.ParseTimestamp(op.Timestamp)
	gtmLag := t.MsLag(ts1, time.Now)
	logFn := func(s sql.Result, e error) {
		log.WithFields(log.Fields{
			"ts":         ts1,
			"ts2":        ts2,
			"msLag":      gtmLag,
			"now":        time.Now().Unix(),
			"action":     op.Operation,
			"id":         op.Id,
			"collection": op.GetCollection(),
			"error":      e,
		}).Debug(fmt.Sprintf("%s worker processed", workerType))
	}
	data := SanitizeData(c.Fields, op).Data
	switch {
	case op.IsInsert():
		t.counters.insert.Incr(1)
		s, err := t.pg.NamedExec(o.BuildUpsert(), data)
		logFn(s, err)
	case op.IsUpdate():
		t.counters.update.Incr(1)
		// Note we're using upsert here vs update
		// This imposes a performance penalty but is more robust
		// in circumstances where an update would fail due to
		// record missing in PG
		s, err := t.pg.NamedExec(o.BuildUpsert(), data)
		logFn(s, err)
	case op.IsDelete() && t.env.allowDeletes:
		t.counters.delete.Incr(1)
		// Deletes have empty op.Data
		// We patch in the op.Id instead for consistent data
		// Bad idea? Should we always rely on op.ID? instead?
		s, err := t.pg.NamedExec(o.BuildDelete(), data)
		logFn(s, err)
	}
}

func OpTimestampWrapper(f func() time.Time, ago time.Duration) func(*mgo.Session, *gtm.Options) bson.MongoTimestamp {
	return func(*mgo.Session, *gtm.Options) bson.MongoTimestamp {
		now := f()
		inPast := now.Add(-ago)
		var c uint32 = 1
		ts, err := NewMongoTimestamp(inPast, c)
		if err != nil {
			log.Error(err)
		}
		return ts
	}
}

func Tail(config Config, pg *sqlx.DB, session *mgo.Session, env Env) {
	supervisor := suture.NewSimple("Supervisor")
	service := NewTailer(config, pg, session, env)
	supervisor.Add(service)
	supervisor.ServeBackground()
	<-service.stop
}
