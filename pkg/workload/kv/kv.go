// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kv

import (
	"context"
	"crypto/sha1"
	gosql "database/sql"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/bufalloc"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/workload"
	"github.com/cockroachdb/cockroach/pkg/workload/histogram"
	"github.com/cockroachdb/errors"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/spf13/pflag"
)

const (
	kvSchema = `(
		k BIGINT NOT NULL PRIMARY KEY,
		v BYTES NOT NULL
	)`
	kvSchemaWithIndex = `(
		k BIGINT NOT NULL PRIMARY KEY,
		v BYTES NOT NULL,
		INDEX (v)
	)`
	// TODO(ajwerner): Change this to use the "easier" hash sharded index syntax once that
	// is in.
	shardedKvSchema = `(
		k BIGINT NOT NULL,
		v BYTES NOT NULL,
		shard INT4 AS (mod(k, %d)) STORED CHECK (%s),
		PRIMARY KEY (shard, k)
	)`
	shardedKvSchemaWithIndex = `(
		k BIGINT NOT NULL,
		v BYTES NOT NULL,
		shard INT4 AS (mod(k, %d)) STORED CHECK (%s),
		PRIMARY KEY (shard, k),
		INDEX (v)
	)`
)

type kv struct {
	flags     workload.Flags
	connFlags *workload.ConnFlags

	batchSize                            int
	minBlockSizeBytes, maxBlockSizeBytes int
	cycleLength                          int64
	readPercent                          int
	spanPercent                          int
	spanLimit                            int
	writesUseSelectForUpdate             bool
	seed                                 int64
	writeSeq                             string
	sequential                           bool
	zipfian                              bool
	splits                               int
	secondaryIndex                       bool
	shards                               int
	targetCompressionRatio               float64
	enum                                 bool
	insertCount                          int
	regionsStr                           string
	regions                              []string
	singleReplica                        bool
	regionIdx                            int
	localRegionPercent                   int
	hot                                  int
	statements                           int
	update                               bool
	sortKeys                             bool
}

func init() {
	workload.Register(kvMeta)
}

var kvMeta = workload.Meta{
	Name:        `kv`,
	Description: `KV reads and writes to keys spread randomly across the cluster.`,
	Details: `
	By default, keys are picked uniformly at random across the cluster.
	--concurrency workers alternate between doing selects and upserts (according
	to a --read-percent ratio). Each select/upsert reads/writes a batch of --batch
	rows. The write keys are randomly generated in a deterministic fashion (or
	sequentially if --sequential is specified). Reads select a random batch of ids
	out of the ones previously written.
	--write-seq can be used to incorporate data produced by a previous run into
	the current run.
	`,
	Version:      `1.0.0`,
	PublicFacing: true,
	New: func() workload.Generator {
		g := &kv{}
		g.flags.FlagSet = pflag.NewFlagSet(`kv`, pflag.ContinueOnError)
		g.flags.Meta = map[string]workload.FlagMeta{
			`batch`: {RuntimeOnly: true},
		}
		g.flags.IntVar(&g.batchSize, `batch`, 1,
			`Number of blocks to read/insert in a single SQL statement.`)
		g.flags.IntVar(&g.minBlockSizeBytes, `min-block-bytes`, 1,
			`Minimum amount of raw data written with each insertion.`)
		g.flags.IntVar(&g.maxBlockSizeBytes, `max-block-bytes`, 1,
			`Maximum amount of raw data written with each insertion`)
		g.flags.Int64Var(&g.cycleLength, `cycle-length`, math.MaxInt64,
			`Number of keys repeatedly accessed by each writer through upserts.`)
		g.flags.IntVar(&g.readPercent, `read-percent`, 0,
			`Percent (0-100) of operations that are reads of existing keys.`)
		g.flags.IntVar(&g.spanPercent, `span-percent`, 0,
			`Percent (0-100) of operations that are spanning queries of all ranges.`)
		g.flags.IntVar(&g.spanLimit, `span-limit`, 0,
			`LIMIT count for each spanning query, or 0 for no limit`)
		g.flags.BoolVar(&g.writesUseSelectForUpdate, `sfu-writes`, false,
			`Use SFU and transactional writes with a sleep after SFU.`)
		g.flags.Int64Var(&g.seed, `seed`, 1, `Key hash seed.`)
		g.flags.BoolVar(&g.zipfian, `zipfian`, false,
			`Pick keys in a zipfian distribution instead of randomly.`)
		g.flags.BoolVar(&g.sequential, `sequential`, false,
			`Pick keys sequentially instead of randomly.`)
		g.flags.StringVar(&g.writeSeq, `write-seq`, "",
			`Initial write sequence value. Can be used to use the data produced by a previous run. `+
				`It has to be of the form (R|S)<number>, where S implies that it was taken from a `+
				`previous --sequential run and R implies a previous random run.`)
		g.flags.IntVar(&g.splits, `splits`, 0,
			`Number of splits to perform before starting normal operations.`)
		g.flags.BoolVar(&g.secondaryIndex, `secondary-index`, false,
			`Add a secondary index to the schema`)
		g.flags.IntVar(&g.shards, `num-shards`, 0,
			`Number of shards to create on the primary key.`)
		g.flags.Float64Var(&g.targetCompressionRatio, `target-compression-ratio`, 1.0,
			`Target compression ratio for data blocks. Must be >= 1.0`)
		g.flags.BoolVar(&g.enum, `enum`, false,
			`Inject an enum column and use it`)
		g.flags.IntVar(&g.insertCount, `insert-count`, 1000, `Number of initial rows`)
		g.flags.StringVar(&g.regionsStr, `regions`, "", `Regions in multi-region deployment`)
		g.flags.BoolVar(&g.singleReplica, `single-replica`, false, `Only have a single replica per region`)
		g.flags.IntVar(&g.regionIdx, `region-idx`, 0, `Region of this client`)
		g.flags.IntVar(&g.localRegionPercent, `local-region-percent`, 100, `Percent of transactions that only access local data`)
		g.flags.IntVar(&g.hot, `hot`, 100, `Number of hot keys per region`)
		g.flags.IntVar(&g.statements, `statements`, 1, `Number of statements. If larger than 1, they are wrapped in a transaction`)
		g.flags.BoolVar(&g.update, `update`, false, `Use update statements`)
		g.flags.BoolVar(&g.sortKeys, `sort-keys`, false, `Sort the keys in a transaction`)
		g.connFlags = workload.NewConnFlags(&g.flags)
		return g
	},
}

// Meta implements the Generator interface.
func (*kv) Meta() workload.Meta { return kvMeta }

// Flags implements the Flagser interface.
func (w *kv) Flags() workload.Flags { return w.flags }

// Hooks implements the Hookser interface.
func (w *kv) Hooks() workload.Hooks {
	return workload.Hooks{
		PreLoad: func(db *gosql.DB) error {
			var err error
			if len(w.regions) > 0 {
				for r, region := range w.regions {
					if r == 0 {
						_, err = db.Exec(fmt.Sprintf(`ALTER DATABASE kv PRIMARY REGION "%s"`, region))
					} else {
						_, err = db.Exec(fmt.Sprintf(`ALTER DATABASE kv ADD REGION "%s"`, region))
					}
					if err != nil {
						return err
					}
				}

				var cases strings.Builder
				for r, region := range w.regions {
					cases.WriteString(fmt.Sprintf(` WHEN mod(k, %d) = %d THEN '%s'`, len(w.regions), r, region))
				}
				regionCol := fmt.Sprintf(`ALTER TABLE kv ADD COLUMN region crdb_internal_region AS (CASE %s END) STORED`, cases.String())
				_, err = db.Exec(regionCol)
				if err != nil {
					return err
				}

				// Set sql.defaults.results_buffer.size = 0 to disable auto-retry for batch request
				_, err = db.Exec(`
				SET CLUSTER SETTING sql.defaults.results_buffer.size = "0";
				ALTER TABLE kv ALTER COLUMN region SET NOT NULL;
				ALTER TABLE kv SET LOCALITY REGIONAL BY ROW AS "region";`)
				if err != nil {
					return err
				}

				if w.singleReplica {
					_, err = db.Exec(`
					SET override_multi_region_zone_config=true;
					ALTER DATABASE kv CONFIGURE ZONE USING constraints='{}';
					ALTER DATABASE kv CONFIGURE ZONE USING num_voters=1;
					ALTER DATABASE kv CONFIGURE ZONE USING num_replicas=1;`)
					if err != nil {
						return err
					}

					for _, region := range w.regions {
						_, err = db.Exec(fmt.Sprintf(`
						SET override_multi_region_zone_config=true;
						ALTER PARTITION "%s" OF INDEX kv.public.kv@primary CONFIGURE ZONE USING num_voters=1`, region))
						if err != nil {
							return err
						}
					}
				}
			}
			return err
		},
		PostLoad: func(db *gosql.DB) error {
			var err error
			if w.enum {
				_, err = db.Exec(`
CREATE TYPE enum_type AS ENUM ('v');
ALTER TABLE kv ADD COLUMN e enum_type NOT NULL AS ('v') STORED;`)
				if err != nil {
					return err
				}
			}
			return err
		},
		Validate: func() error {
			if w.maxBlockSizeBytes < w.minBlockSizeBytes {
				return errors.Errorf("Value of 'max-block-bytes' (%d) must be greater than or equal to value of 'min-block-bytes' (%d)",
					w.maxBlockSizeBytes, w.minBlockSizeBytes)
			}
			if w.sequential && w.splits > 0 {
				return errors.New("'sequential' and 'splits' cannot both be enabled")
			}
			if w.sequential && w.zipfian {
				return errors.New("'sequential' and 'zipfian' cannot both be enabled")
			}
			if w.readPercent+w.spanPercent > 100 {
				return errors.New("'read-percent' and 'span-percent' higher than 100")
			}
			if w.targetCompressionRatio < 1.0 || math.IsNaN(w.targetCompressionRatio) {
				return errors.New("'target-compression-ratio' must be a number >= 1.0")
			}

			regionsStr := strings.TrimSpace(w.regionsStr)
			if regionsStr != "" {
				w.regions = strings.Split(regionsStr, ",")
			}
			return nil
		},
	}
}

var tableTypes = []*types.T{types.Int, types.Bytes}

// Tables implements the Generator interface.
func (w *kv) Tables() []workload.Table {
	table := workload.Table{
		Name: `kv`,
		// TODO(dan): Support initializing kv with data.
		Splits: workload.Tuples(
			w.splits,
			func(splitIdx int) []interface{} {
				stride := (float64(w.cycleLength) - float64(math.MinInt64)) / float64(w.splits+1)
				splitPoint := int(math.MinInt64 + float64(splitIdx+1)*stride)
				return []interface{}{splitPoint}
			},
		),
	}
	if w.shards > 0 {
		schema := shardedKvSchema
		if w.secondaryIndex {
			schema = shardedKvSchemaWithIndex
		}
		checkConstraint := strings.Builder{}
		checkConstraint.WriteString(`shard IN (`)
		for i := 0; i < w.shards; i++ {
			if i != 0 {
				checkConstraint.WriteString(",")
			}
			fmt.Fprintf(&checkConstraint, "%d", i)
		}
		checkConstraint.WriteString(")")
		table.Schema = fmt.Sprintf(schema, w.shards, checkConstraint.String())
	} else {
		if w.secondaryIndex {
			table.Schema = kvSchemaWithIndex
		} else {
			table.Schema = kvSchema
		}
	}

	const batchSize = 1000
	table.InitialRows = workload.BatchedTuples{
		NumBatches: (w.insertCount + batchSize - 1) / batchSize,
		FillBatch: func(batchIdx int, cb coldata.Batch, _ *bufalloc.ByteAllocator) {
			rowBegin, rowEnd := batchIdx*batchSize, (batchIdx+1)*batchSize
			if rowEnd > w.insertCount {
				rowEnd = w.insertCount
			}
			cb.Reset(tableTypes, rowEnd-rowBegin, coldata.StandardColumnFactory)

			key := cb.ColVec(0).Int64()
			value := cb.ColVec(1).Bytes()
			value.Reset()

			rng := rand.New(rand.NewSource(w.seed + int64(batchIdx)))
			for rowIdx := rowBegin; rowIdx < rowEnd; rowIdx++ {
				rowOffset := rowIdx - rowBegin
				key[rowOffset] = int64(rowIdx)
				value.Set(rowOffset, randomBlock(w, rng))
			}
		},
	}

	return []workload.Table{table}
}

// Ops implements the Opser interface.
func (w *kv) Ops(
	ctx context.Context, urls []string, reg *histogram.Registry,
) (workload.QueryLoad, error) {
	writeSeq := 0
	if w.writeSeq != "" {
		first := w.writeSeq[0]
		if len(w.writeSeq) < 2 || (first != 'R' && first != 'S') {
			return workload.QueryLoad{}, fmt.Errorf("--write-seq has to be of the form '(R|S)<num>'")
		}
		rest := w.writeSeq[1:]
		var err error
		writeSeq, err = strconv.Atoi(rest)
		if err != nil {
			return workload.QueryLoad{}, fmt.Errorf("--write-seq has to be of the form '(R|S)<num>'")
		}
		if first == 'R' && w.sequential {
			return workload.QueryLoad{}, fmt.Errorf("--sequential incompatible with a Random --write-seq")
		}
		if first == 'S' && !w.sequential {
			return workload.QueryLoad{}, fmt.Errorf(
				"--sequential=false incompatible with a Sequential --write-seq")
		}
	}

	sqlDatabase, err := workload.SanitizeUrls(w, w.connFlags.DBOverride, urls)
	if err != nil {
		return workload.QueryLoad{}, err
	}
	cfg := workload.MultiConnPoolCfg{
		MaxTotalConnections: w.connFlags.Concurrency + 1,
	}
	mcp, err := workload.NewMultiConnPool(ctx, cfg, urls...)
	if err != nil {
		return workload.QueryLoad{}, err
	}

	// Read statement
	var buf strings.Builder
	if w.shards == 0 {
		buf.WriteString(`SELECT k, v FROM kv WHERE k IN (`)
		for i := 0; i < w.batchSize; i++ {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, `$%d`, i+1)
		}
	} else if w.enum {
		buf.WriteString(`SELECT k, v, e FROM kv WHERE k IN (`)
		for i := 0; i < w.batchSize; i++ {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, `$%d`, i+1)
		}
	} else {
		// TODO(ajwerner): We're currently manually plumbing down the computed shard column
		// since the optimizer doesn't yet support deriving values of computed columns
		// when all the columns they reference are available. See
		// https://github.com/cockroachdb/cockroach/issues/39340#issuecomment-535338071
		// for details. Remove this once that functionality is added.
		buf.WriteString(`SELECT k, v FROM kv WHERE (shard, k) in (`)
		for i := 0; i < w.batchSize; i++ {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, `(mod($%d, %d), $%d)`, i+1, w.shards, i+1)
		}
	}
	buf.WriteString(`)`)
	readStmtStr := buf.String()

	// Write statement
	buf.Reset()
	buf.WriteString(`UPSERT INTO kv (k, v) VALUES`)
	for i := 0; i < w.batchSize; i++ {
		j := i * 2
		if i > 0 {
			buf.WriteString(", ")
		}
		fmt.Fprintf(&buf, ` ($%d, $%d)`, j+1, j+2)
	}
	writeStmtStr := buf.String()

	buf.Reset()
	buf.WriteString(`UPDATE kv SET v = $1 WHERE k IN (`)
	for i := 0; i < w.batchSize; i++ {
		if i > 0 {
			buf.WriteString(", ")
		}
		fmt.Fprintf(&buf, `$%d`, i+2)
	}
	buf.WriteString(`)`)
	updateStmtStr := buf.String()

	// Select for update statement
	var sfuStmtStr string
	if w.writesUseSelectForUpdate {
		if w.shards != 0 {
			return workload.QueryLoad{}, fmt.Errorf("select for update in kv requires shard=0")
		}
		buf.Reset()
		buf.WriteString(`SELECT k, v FROM kv WHERE k IN (`)
		for i := 0; i < w.batchSize; i++ {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, `$%d`, i+1)
		}
		buf.WriteString(`) FOR UPDATE`)
		sfuStmtStr = buf.String()
	}

	// Span statement
	buf.Reset()
	buf.WriteString(`SELECT count(v) FROM [SELECT v FROM kv`)
	if w.spanLimit > 0 {
		fmt.Fprintf(&buf, ` ORDER BY k LIMIT %d`, w.spanLimit)
	}
	buf.WriteString(`]`)
	spanStmtStr := buf.String()

	ql := workload.QueryLoad{SQLDatabase: sqlDatabase}
	seq := &sequence{config: w, val: int64(writeSeq)}
	numEmptyResults := new(int64)
	for i := 0; i < w.connFlags.Concurrency; i++ {
		op := &kvOp{
			config:          w,
			hists:           reg.GetHandle(),
			numEmptyResults: numEmptyResults,
		}
		op.readStmt = op.sr.Define(readStmtStr)
		op.writeStmt = op.sr.Define(writeStmtStr)
		op.updateStmt = op.sr.Define(updateStmtStr)
		if len(sfuStmtStr) > 0 {
			op.sfuStmt = op.sr.Define(sfuStmtStr)
		}
		op.spanStmt = op.sr.Define(spanStmtStr)
		if err := op.sr.Init(ctx, "kv", mcp, w.connFlags); err != nil {
			return workload.QueryLoad{}, err
		}
		op.mcp = mcp
		if w.sequential {
			op.g = newSequentialGenerator(seq)
		} else if w.zipfian {
			op.g = newZipfianGenerator(seq)
		} else {
			op.g = newHashGenerator(seq)
		}
		op.mg = nil
		if len(w.regions) > 1 {
			op.mg = newMultiRegionKeyGenerator(w)
		}
		ql.WorkerFns = append(ql.WorkerFns, op.run)
		ql.Close = op.close
	}
	return ql, nil
}

type kvOp struct {
	config          *kv
	hists           *histogram.Histograms
	sr              workload.SQLRunner
	mcp             *workload.MultiConnPool
	readStmt        workload.StmtHandle
	writeStmt       workload.StmtHandle
	spanStmt        workload.StmtHandle
	sfuStmt         workload.StmtHandle
	updateStmt      workload.StmtHandle
	g               keyGenerator
	numEmptyResults *int64 // accessed atomically
	mg              *multiRegionKeyGenerator
}

var reAbortReasons = []*regexp.Regexp{
	regexp.MustCompile(`ABORT_[A-Z_]+`),
	regexp.MustCompile(`RETRY_[A-Z]_+`),
	regexp.MustCompile(`ReadWithinUncertaintyInterval`),
	regexp.MustCompile(`WriteTooOldError`),
}

func (o *kvOp) run(ctx context.Context) (retErr error) {
	statementProbability := o.g.rand().Intn(100) // Determines what statement is executed.
	if statementProbability < o.config.readPercent {
		args := make([]interface{}, o.config.batchSize)
		for i := 0; i < o.config.batchSize; i++ {
			args[i] = o.g.readKey()
		}
		start := timeutil.Now()
		rows, err := o.readStmt.Query(ctx, args...)
		if err != nil {
			return err
		}
		empty := true
		for rows.Next() {
			empty = false
		}
		if empty {
			atomic.AddInt64(o.numEmptyResults, 1)
		}
		elapsed := timeutil.Since(start)
		o.hists.Get(`read`).Record(elapsed)
		return rows.Err()
	}
	// Since we know the statement is not a read, we recalibrate
	// statementProbability to only consider the other statements.
	statementProbability -= o.config.readPercent
	if statementProbability < o.config.spanPercent {
		start := timeutil.Now()
		_, err := o.spanStmt.Exec(ctx)
		elapsed := timeutil.Since(start)
		o.hists.Get(`span`).Record(elapsed)
		return err
	}

	start := timeutil.Now()
	if o.config.update {
		var err error
		updateArgs := make([]interface{}, (1+o.config.batchSize)*o.config.statements)
		var keys []int
		if o.mg != nil {
			keys, err = o.mg.keys(o.config.statements * o.config.batchSize)
			if err != nil {
				return err
			}
		}

		if o.config.sortKeys {
			sort.Ints(keys)
		}

		for s := 0; s < o.config.statements; s++ {
			offset := s * (1 + o.config.batchSize)
			updateArgs[offset] = randomBlock(o.config, o.g.rand())
			for i := 0; i < o.config.batchSize; i++ {
				j := offset + i + 1
				if o.mg != nil {
					updateArgs[j] = keys[s*o.config.batchSize+i]
				} else {
					updateArgs[j] = o.g.writeKey()
				}
			}
		}

		batch := &pgx.Batch{}
		batch.Queue("BEGIN")
		// Produce some out put so that the batch request won't be automatically retried
		batch.Queue("SELECT 1")
		for s := 0; s < o.config.statements; s++ {
			from := s * (1 + o.config.batchSize)
			to := from + (1 + o.config.batchSize)
			batch.Queue(o.updateStmt.PreparedName(), updateArgs[from:to]...)
		}
		batch.Queue("COMMIT")

		batchResults := o.mcp.Get().SendBatch(ctx, batch)
		if err := batchResults.Close(); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				if pgcode.MakeCode(pgErr.Code) == pgcode.SerializationFailure {
					for _, re := range reAbortReasons {
						reason := re.Find([]byte(pgErr.Message))
						if reason != nil {
							return o.tryHandleWriteErr(re.String(), start, err)
						}
					}
				}
			}
			return o.tryHandleWriteErr("commit-err", start, err)
		}
		elapsed := timeutil.Since(start)
		o.hists.Get(`write`).Record(elapsed)
		return err
	}

	const argCount = 2
	writeArgs := make([]interface{}, argCount*o.config.batchSize)
	var sfuArgs []interface{}
	if o.config.writesUseSelectForUpdate {
		sfuArgs = make([]interface{}, o.config.batchSize)
	}

	if o.mg != nil {
		keys, err := o.mg.keys(o.config.batchSize)
		if err != nil {
			return err
		}
		for i := 0; i < o.config.batchSize; i++ {
			j := i * argCount
			writeArgs[j+0] = keys[i]
			if sfuArgs != nil {
				sfuArgs[i] = writeArgs[j]
			}
			writeArgs[j+1] = randomBlock(o.config, o.g.rand())
		}
	} else {
		for i := 0; i < o.config.batchSize; i++ {
			j := i * argCount
			writeArgs[j+0] = o.g.writeKey()
			if sfuArgs != nil {
				sfuArgs[i] = writeArgs[j]
			}
			writeArgs[j+1] = randomBlock(o.config, o.g.rand())
		}
	}

	var err error
	if o.config.writesUseSelectForUpdate {
		// We could use crdb.ExecuteTx, but we avoid retries in this workload so
		// that each run call makes 1 attempt, so that rate limiting in workerRun
		// behaves as expected.
		var tx pgx.Tx
		if tx, err = o.mcp.Get().Begin(ctx); err != nil {
			return err
		}
		defer func() {
			rollbackErr := tx.Rollback(ctx)
			if !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				retErr = errors.CombineErrors(retErr, rollbackErr)
			}
		}()
		rows, err := o.sfuStmt.QueryTx(ctx, tx, sfuArgs...)
		if err != nil {
			return err
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		// Simulate a transaction that does other work between the SFU and write.
		// TODO(sumeer): this should be configurable.
		time.Sleep(10 * time.Millisecond)
		if _, err = o.writeStmt.ExecTx(ctx, tx, writeArgs...); err != nil {
			// Multiple write transactions can contend and encounter
			// a serialization failure. We swallow such an error.
			return o.tryHandleWriteErr("write-write-err", start, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return o.tryHandleWriteErr("write-commit-err", start, err)
		}
	} else {
		_, err = o.writeStmt.Exec(ctx, writeArgs...)
	}
	elapsed := timeutil.Since(start)
	o.hists.Get(`write`).Record(elapsed)
	return err
}

func (o *kvOp) tryHandleWriteErr(name string, start time.Time, err error) error {
	// If the error is not an instance of pgconn.PgError, then it is unexpected.
	pgErr := new(pgconn.PgError)
	if !errors.As(err, &pgErr) {
		return err
	}
	// Transaction retry errors are acceptable. Allow the transaction
	// to rollback.
	if pgcode.MakeCode(pgErr.Code) == pgcode.SerializationFailure {
		elapsed := timeutil.Since(start)
		o.hists.Get(name).Record(elapsed)
		return nil
	}
	return err
}

func (o *kvOp) close(context.Context) {
	if empty := atomic.LoadInt64(o.numEmptyResults); empty != 0 {
		fmt.Printf("Number of reads that didn't return any results: %d.\n", empty)
	}
	seq := o.g.sequence()
	var ch string
	if o.config.sequential {
		ch = "S"
	} else {
		ch = "R"
	}
	fmt.Printf("Highest sequence written: %d. Can be passed as --write-seq=%s%d to the next run.\n",
		seq, ch, seq)
}

type sequence struct {
	config *kv
	val    int64
}

func (s *sequence) write() int64 {
	return (atomic.AddInt64(&s.val, 1) - 1) % s.config.cycleLength
}

// read returns the last key index that has been written. Note that the returned
// index might not actually have been written yet, so a read operation cannot
// require that the key is present.
func (s *sequence) read() int64 {
	return atomic.LoadInt64(&s.val) % s.config.cycleLength
}

// keyGenerator generates read and write keys. Read keys may not yet exist and
// write keys may already exist.
type keyGenerator interface {
	writeKey() int64
	readKey() int64
	rand() *rand.Rand
	sequence() int64
}

type hashGenerator struct {
	seq    *sequence
	random *rand.Rand
	hasher hash.Hash
	buf    [sha1.Size]byte
}

func newHashGenerator(seq *sequence) *hashGenerator {
	return &hashGenerator{
		seq:    seq,
		random: rand.New(rand.NewSource(timeutil.Now().UnixNano())),
		hasher: sha1.New(),
	}
}

func (g *hashGenerator) hash(v int64) int64 {
	binary.BigEndian.PutUint64(g.buf[:8], uint64(v))
	binary.BigEndian.PutUint64(g.buf[8:16], uint64(g.seq.config.seed))
	g.hasher.Reset()
	_, _ = g.hasher.Write(g.buf[:16])
	g.hasher.Sum(g.buf[:0])
	return int64(binary.BigEndian.Uint64(g.buf[:8]))
}

func (g *hashGenerator) writeKey() int64 {
	return g.hash(g.seq.write())
}

func (g *hashGenerator) readKey() int64 {
	v := g.seq.read()
	if v == 0 {
		return 0
	}
	return g.hash(g.random.Int63n(v))
}

func (g *hashGenerator) rand() *rand.Rand {
	return g.random
}

func (g *hashGenerator) sequence() int64 {
	return atomic.LoadInt64(&g.seq.val)
}

type sequentialGenerator struct {
	seq    *sequence
	random *rand.Rand
}

func newSequentialGenerator(seq *sequence) *sequentialGenerator {
	return &sequentialGenerator{
		seq:    seq,
		random: rand.New(rand.NewSource(timeutil.Now().UnixNano())),
	}
}

func (g *sequentialGenerator) writeKey() int64 {
	return g.seq.write()
}

func (g *sequentialGenerator) readKey() int64 {
	v := g.seq.read()
	if v == 0 {
		return 0
	}
	return g.random.Int63n(v)
}

func (g *sequentialGenerator) rand() *rand.Rand {
	return g.random
}

func (g *sequentialGenerator) sequence() int64 {
	return atomic.LoadInt64(&g.seq.val)
}

type zipfGenerator struct {
	seq    *sequence
	random *rand.Rand
	zipf   *zipf
}

// Creates a new zipfian generator.
func newZipfianGenerator(seq *sequence) *zipfGenerator {
	random := rand.New(rand.NewSource(timeutil.Now().UnixNano()))
	return &zipfGenerator{
		seq:    seq,
		random: random,
		zipf:   newZipf(1.1, 1, uint64(math.MaxInt64)),
	}
}

// Get a random number seeded by v that follows the
// zipfian distribution.
func (g *zipfGenerator) zipfian(seed int64) int64 {
	randomWithSeed := rand.New(rand.NewSource(seed))
	return int64(g.zipf.Uint64(randomWithSeed))
}

// Get a zipf write key appropriately.
func (g *zipfGenerator) writeKey() int64 {
	return g.zipfian(g.seq.write())
}

// Get a zipf read key appropriately.
func (g *zipfGenerator) readKey() int64 {
	v := g.seq.read()
	if v == 0 {
		return 0
	}
	return g.zipfian(g.random.Int63n(v))
}

func (g *zipfGenerator) rand() *rand.Rand {
	return g.random
}

func (g *zipfGenerator) sequence() int64 {
	return atomic.LoadInt64(&g.seq.val)
}

type multiRegionKeyGenerator struct {
	config *kv
	r      *rand.Rand
}

func newMultiRegionKeyGenerator(config *kv) *multiRegionKeyGenerator {
	r := rand.New(rand.NewSource(timeutil.Now().UnixNano()))
	return &multiRegionKeyGenerator{
		config: config,
		r:      r,
	}
}

func (g *multiRegionKeyGenerator) keys(numKeys int) ([]int, error) {
	numRegions := len(g.config.regions)
	localRegion := g.config.regionIdx
	localRegionProb := g.r.Intn(100)

	var keys []int
	var err error
	if localRegionProb < g.config.localRegionPercent {
		keys, err = g.keyBatch(numKeys, 2)
		if err != nil {
			return []int{}, err
		}
		for i := 0; i < len(keys); i++ {
			keys[i] = keys[i]*numRegions + localRegion
		}
	} else {
		remoteRegion := (g.config.regionIdx + g.r.Intn(numRegions-1) + 1) % numRegions
		numLocalKeys := numKeys / 2
		keys, err = g.keyBatch(numLocalKeys, 1)
		if err != nil {
			return []int{}, err
		}
		for i := 0; i < len(keys); i++ {
			keys[i] = keys[i]*numRegions + localRegion
		}

		remoteKeys, err := g.keyBatch(numKeys-numLocalKeys, 1)
		if err != nil {
			return []int{}, err
		}
		for i := 0; i < len(remoteKeys); i++ {
			keys = append(keys, remoteKeys[i]*numRegions+remoteRegion)
		}
	}

	return keys, nil
}

func (g *multiRegionKeyGenerator) keyBatch(n int, numFromHot int) ([]int, error) {
	if n < 1 {
		return []int{}, errors.New("Number of keys must be at least 1")
	}

	numRegions := len(g.config.regions)
	numKeysPerRegion := g.config.insertCount / numRegions
	hot := g.config.hot

	if numKeysPerRegion < hot {
		return []int{}, errors.New("Number of keys per region must be larger than or equal to HOT")
	}

	keys := make([]int, n)
	keyMap := make(map[int]struct{})
	i := 0

	for ; i < numFromHot; i++ {
		for {
			keys[i] = g.r.Intn(hot)
			_, exists := keyMap[keys[i]]
			if !exists {
				break
			}
		}
		keyMap[keys[i]] = struct{}{}
	}

	for ; i < n; i++ {
		for {
			keys[i] = g.r.Intn(numKeysPerRegion-hot) + hot
			_, exists := keyMap[keys[i]]
			if !exists {
				break
			}
		}
		keyMap[keys[i]] = struct{}{}
	}

	return keys, nil
}

func randomBlock(config *kv, r *rand.Rand) []byte {
	blockSize := r.Intn(config.maxBlockSizeBytes-config.minBlockSizeBytes+1) + config.minBlockSizeBytes
	blockData := make([]byte, blockSize)
	uniqueSize := int(float64(blockSize) / config.targetCompressionRatio)
	if uniqueSize < 1 {
		uniqueSize = 1
	}
	for i := range blockData {
		if i >= uniqueSize {
			blockData[i] = blockData[i-uniqueSize]
		} else {
			blockData[i] = byte(r.Int() & 0xff)
		}
	}
	return blockData
}
