package sling

import (
	"bufio"
	"context"
	"database/sql"
	"io/ioutil"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/flarco/dbio"
	"github.com/samber/lo"

	"github.com/flarco/dbio/saas/airbyte"

	"github.com/flarco/dbio/filesys"

	"github.com/dustin/go-humanize"
	"github.com/slingdata-io/sling-cli/core/env"

	"github.com/flarco/dbio/database"
	"github.com/flarco/dbio/iop"
	"github.com/flarco/g"
	"github.com/spf13/cast"
)

// AllowedProps allowed properties
var AllowedProps = map[string]string{
	"sheet": "Provided for Excel source files. Default is first sheet",
	"range": "Optional for Excel source file. Default is largest table range",
}

var start time.Time
var PermitTableSchemaOptimization = false
var MetadataLoadedAt = false
var MetadataStreamURL = false
var slingLoadedAtColumn = "_sling_loaded_at"
var slingStreamURLColumn = "_sling_stream_url"

func init() {
	if val := os.Getenv("SLING_LOADED_AT_COLUMN"); val != "" {
		MetadataLoadedAt = cast.ToBool(val)
	}
	if val := os.Getenv("SLING_STREAM_URL_COLUMN"); val != "" {
		MetadataStreamURL = cast.ToBool(val)
	}
}

// IsStalled determines if the task has stalled (no row increment)
func (t *TaskExecution) IsStalled(window float64) bool {
	if strings.Contains(t.Progress, "pre-sql") || strings.Contains(t.Progress, "post-sql") {
		return false
	}
	return time.Since(t.lastIncrement).Seconds() > window
}

// GetBytes return the current total of bytes processed
func (t *TaskExecution) GetBytes() (inBytes, outBytes uint64) {
	inBytes, outBytes = t.df.Bytes()
	if inBytes == 0 && outBytes == 0 {
		// use tx/rc bytes
		// stats := g.GetProcStats(os.Getpid())
		// inBytes = stats.RcBytes - t.ProcStatsStart.RcBytes
		// outBytes = stats.TxBytes - t.ProcStatsStart.TxBytes
	}
	return
}

func (t *TaskExecution) GetBytesString() (s string) {
	inBytes, _ := t.GetBytes()
	if inBytes == 0 {
		return ""
	}
	return g.F("%s", humanize.Bytes(inBytes))
	// if inBytes > 0 && inBytes == outBytes {
	// 	return g.F("%s", humanize.Bytes(inBytes))
	// }
	// return g.F("%s -> %s", humanize.Bytes(inBytes), humanize.Bytes(outBytes))
}

// GetCount return the current count of rows processed
func (t *TaskExecution) GetCount() (count uint64) {
	if t.StartTime == nil {
		return
	}

	return t.df.Count()
}

// GetRate return the speed of flow (rows / sec and bytes / sec)
// secWindow is how many seconds back to measure (0 is since beginning)
func (t *TaskExecution) GetRate(secWindow int) (rowRate, byteRate int64) {
	var secElapsed float64
	count := t.GetCount()
	bytes, _ := t.GetBytes()
	if t.StartTime == nil || t.StartTime.IsZero() {
		return
	} else if t.EndTime == nil || t.EndTime.IsZero() {
		st := *t.StartTime
		if secWindow <= 0 {
			secElapsed = time.Since(st).Seconds()
			rowRate = cast.ToInt64(math.Round(cast.ToFloat64(count) / secElapsed))
			byteRate = cast.ToInt64(math.Round(cast.ToFloat64(bytes) / secElapsed))
		} else {
			rowRate = cast.ToInt64(math.Round(cast.ToFloat64((count - t.prevRowCount) / cast.ToUint64(secWindow))))
			byteRate = cast.ToInt64(math.Round(cast.ToFloat64((bytes - t.prevByteCount) / cast.ToUint64(secWindow))))
			if t.prevRowCount < count {
				t.lastIncrement = time.Now()
			}
			t.prevRowCount = count
			t.prevByteCount = bytes
		}
	} else {
		st := *t.StartTime
		et := *t.EndTime
		secElapsed = cast.ToFloat64(et.UnixNano()-st.UnixNano()) / 1000000000.0
		rowRate = cast.ToInt64(math.Round(cast.ToFloat64(count) / secElapsed))
		byteRate = cast.ToInt64(math.Round(cast.ToFloat64(bytes) / secElapsed))
	}
	return
}

// Execute runs a Sling task.
// This may be a file/db to file/db transfer
func (t *TaskExecution) Execute() error {
	env.InitLogger()

	done := make(chan struct{})
	now := time.Now()
	t.StartTime = &now
	t.lastIncrement = now

	if t.Context == nil {
		ctx := g.NewContext(context.Background())
		t.Context = &ctx
	}

	// get stats of process at beginning
	t.ProcStatsStart = g.GetProcStats(os.Getpid())

	// set deaults
	t.Config.SetDefault()

	// print for debugging
	g.Trace("using Config:\n%s", g.Pretty(t.Config))
	go func() {
		defer close(done)
		defer t.PBar.Finish()

		t.Status = ExecStatusRunning

		if t.Err != nil {
			return
		}

		g.Debug("type is %s", t.Type)
		switch t.Type {
		case DbSQL:
			t.Err = t.runDbSQL()
		case FileToDB:
			t.Err = t.runFileToDB()
		case DbToDb:
			t.Err = t.runDbToDb()
		case DbToFile:
			t.Err = t.runDbToFile()
		case FileToFile:
			t.Err = t.runFileToFile()
		case APIToDb:
			t.Err = t.runAPIToDB()
		case APIToFile:
			t.Err = t.runAPIToFile()
		default:
			t.SetProgress("task execution configuration is invalid")
			t.Err = g.Error("Cannot Execute. Task Type is not specified")
		}
	}()

	select {
	case <-done:
		t.Cleanup()
	case <-t.Context.Ctx.Done():
		go t.Cleanup()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if t.Err == nil {
			t.Err = g.Error("Execution interrupted")
		}
	}

	if t.Err == nil {
		t.SetProgress("execution succeeded")
		t.Status = ExecStatusSuccess
	} else {
		t.SetProgress("execution failed")
		t.Status = ExecStatusError
		t.Err = g.Error(t.Err, "execution failed")
	}

	now2 := time.Now()
	t.EndTime = &now2

	return t.Err
}

func (t *TaskExecution) getMetadata() (metadata iop.Metadata) {
	if MetadataLoadedAt {
		metadata.LoadedAt.Key = slingLoadedAtColumn
		metadata.LoadedAt.Value = t.StartTime.Unix()
	}
	if MetadataStreamURL {
		metadata.StreamURL.Key = slingStreamURLColumn
	}
	return metadata
}

func (t *TaskExecution) getSrcDBConn() (conn database.Connection, err error) {
	options := g.M()
	g.Unmarshal(g.Marshal(t.Config.Source.Options), &options)
	options["METADATA"] = g.Marshal(t.getMetadata())
	srcProps := append(
		g.MapToKVArr(t.Config.SrcConn.DataS()),
		g.MapToKVArr(g.ToMapString(options))...,
	)
	conn, err = database.NewConnContext(t.Context.Ctx, t.Config.SrcConn.URL(), srcProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize source connection")
		return
	}
	return
}

func (t *TaskExecution) getTgtDBConn() (conn database.Connection, err error) {
	options := g.M()
	g.Unmarshal(g.Marshal(t.Config.Target.Options), &options)
	tgtProps := append(
		g.MapToKVArr(t.Config.TgtConn.DataS()), g.MapToKVArr(g.ToMapString(options))...,
	)
	conn, err = database.NewConnContext(t.Context.Ctx, t.Config.TgtConn.URL(), tgtProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	// set bulk
	if t.Config.Target.Options.UseBulk == g.Bool(false) {
		conn.SetProp("use_bulk", "false")
		conn.SetProp("allow_bulk_import", "false")
	}
	return
}

func (t *TaskExecution) runDbSQL() (err error) {

	start = time.Now()

	tgtConn, err := t.getTgtDBConn()
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	t.SetProgress("connecting to target database (%s)", tgtConn.GetType())
	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Config.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer tgtConn.Close()

	t.SetProgress("executing sql on target database")
	result, err := tgtConn.Exec(t.Config.Target.Object)
	if err != nil {
		err = g.Error(err, "Could not complete sql execution on %s (%s)", t.Config.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	rowAffCnt, err := result.RowsAffected()
	if err == nil {
		t.SetProgress("%d rows affected", rowAffCnt)
	}

	return
}

func (t *TaskExecution) runDbToFile() (err error) {

	start = time.Now()

	srcConn, err := t.getSrcDBConn()
	if err != nil {
		err = g.Error(err, "Could not initialize source connection")
		return
	}

	t.SetProgress("connecting to source database (%s)", srcConn.GetType())
	err = srcConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Config.SrcConn.Info().Name, srcConn.GetType())
		return
	}

	defer srcConn.Close()

	t.SetProgress("reading from source database")
	defer t.Cleanup()
	t.df, err = t.ReadFromDB(t.Config, srcConn)
	if err != nil {
		err = g.Error(err, "Could not ReadFromDB")
		return
	}
	defer t.df.Close()

	if t.Config.Options.StdOut {
		t.SetProgress("writing to target stream (stdout)")
	} else {
		t.SetProgress("writing to target file system (%s)", t.Config.TgtConn.Type)
	}
	cnt, err := t.WriteToFile(t.Config, t.df)
	if err != nil {
		err = g.Error(err, "Could not WriteToFile")
		return
	}

	t.SetProgress("wrote %d rows [%s r/s]", cnt, getRate(cnt))

	err = t.df.Context.Err()
	return

}

func (t *TaskExecution) runAPIToFile() (err error) {

	start = time.Now()

	t.SetProgress("connecting to source api system (%s)", t.Config.SrcConn.Info().Type)
	srcConn, err := t.Config.SrcConn.AsAirbyte()
	if err != nil {
		err = g.Error(err, "Could not obtain client for: %s", t.Config.SrcConn.Type)
		return err
	}
	defer srcConn.Close()

	err = srcConn.Init(false)
	if err != nil {
		err = g.Error(err, "Could not init connection for: %s", t.Config.SrcConn.Type)
		return err
	}
	srcConn.SetProp("METADATA", g.Marshal(t.getMetadata()))

	t.SetProgress("reading from source api system (%s)", t.Config.SrcConn.Type)
	t.df, err = t.ReadFromAPI(t.Config, srcConn)
	if err != nil {
		err = g.Error(err, "could not read from api")
		return
	}
	defer t.df.Close()

	if t.Config.Options.StdOut {
		t.SetProgress("writing to target stream (stdout)")
	} else {
		t.SetProgress("writing to target file system (%s)", t.Config.TgtConn.Type)
	}
	defer t.Cleanup()
	cnt, err := t.WriteToFile(t.Config, t.df)
	if err != nil {
		err = g.Error(err, "Could not WriteToFile")
		return
	}

	t.SetProgress("wrote %d rows [%s r/s]", cnt, getRate(cnt))

	err = t.df.Context.Err()
	return

}

func (t *TaskExecution) runFolderToDB() (err error) {
	/*
		This will take a URL as a folder path
		1. list the files/folders in it (not recursive)
		2a. run runFileToDB for each of the files, naming the target table respectively
		2b. OR run runFileToDB for each of the files, to the same target able, assume each file has same structure
		3. keep list of file inserted in Job.Settings (view handleExecutionHeartbeat in server_ws.go).

	*/
	return
}

func (t *TaskExecution) runAPIToDB() (err error) {

	start = time.Now()

	t.SetProgress("connecting to source api system (%s)", t.Config.SrcConn.Info().Type)
	srcConn, err := t.Config.SrcConn.AsAirbyte()
	if err != nil {
		err = g.Error(err, "Could not obtain client for: %s", t.Config.SrcConn.Type)
		return err
	}

	err = srcConn.Init(false)
	if err != nil {
		err = g.Error(err, "Could not init connection for: %s", t.Config.SrcConn.Type)
		return err
	}
	srcConn.SetProp("METADATA", g.Marshal(t.getMetadata()))

	tgtConn, err := t.getTgtDBConn()
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	t.SetProgress("connecting to target database (%s)", tgtConn.GetType())
	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Config.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer tgtConn.Close()
	defer srcConn.Close()

	// set schema if needed
	t.Config.Target.Object = setSchema(cast.ToString(t.Config.Target.Data["schema"]), t.Config.Target.Object)
	t.Config.Target.Options.TableTmp = setSchema(cast.ToString(t.Config.Target.Data["schema"]), t.Config.Target.Options.TableTmp)

	// get watermark
	if t.usingCheckpoint() {
		t.SetProgress("getting checkpoint value")
		dateLayout := iop.Iso8601ToGoLayout(srcConn.GetProp("date_layout"))
		varMap := map[string]string{
			"timestamp_layout":     dateLayout,
			"timestamp_layout_str": "{value}",
			"date_layout":          dateLayout,
			"date_layout_str":      "{value}",
		}
		t.Config.IncrementalVal, err = getIncrementalValue(t.Config, tgtConn, varMap)
		if err != nil {
			err = g.Error(err, "Could not get incremental value")
			return err
		}
	}

	t.SetProgress("reading from source api system (%s)", t.Config.SrcConn.Type)
	t.df, err = t.ReadFromAPI(t.Config, srcConn)
	if err != nil {
		err = g.Error(err, "could not read from api")
		return
	}
	defer t.df.Close()

	t.SetProgress("writing to target database [mode: %s]", t.Config.Mode)
	defer t.Cleanup()
	cnt, err := t.WriteToDb(t.Config, t.df, tgtConn)
	if err != nil {
		err = g.Error(err, "could not write to database")
		if t.Config.Target.TmpTableCreated {
			// need to drop residue
			tgtConn.DropTable(t.Config.Target.Options.TableTmp)
		}
		return
	}

	elapsed := int(time.Since(start).Seconds())
	t.SetProgress("inserted %d rows in %d secs [%s r/s]", cnt, elapsed, getRate(cnt))

	if err != nil {
		err = g.Error(t.df.Err(), "error in transfer")
	}
	return
}

func (t *TaskExecution) runFileToDB() (err error) {

	start = time.Now()

	tgtConn, err := t.getTgtDBConn()
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	t.SetProgress("connecting to target database (%s)", tgtConn.GetType())
	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Config.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer tgtConn.Close()

	if t.usingCheckpoint() {
		t.SetProgress("getting checkpoint value")
		t.Config.Source.UpdateKey = slingLoadedAtColumn
		varMap := map[string]string{} // should always be number
		t.Config.IncrementalVal, err = getIncrementalValue(t.Config, tgtConn, varMap)
		if err != nil {
			err = g.Error(err, "Could not get incremental value")
			return err
		}
	}

	if t.Config.Options.StdIn {
		t.SetProgress("reading from stream (stdin)")
	} else {
		t.SetProgress("reading from source file system (%s)", t.Config.SrcConn.Type)
	}
	t.df, err = t.ReadFromFile(t.Config)
	if err != nil {
		if strings.Contains(err.Error(), "Provided 0 files") {
			if t.usingCheckpoint() && t.Config.IncrementalVal != "" {
				t.SetProgress("no new files found since latest timestamp (%s)", time.Unix(cast.ToInt64(t.Config.IncrementalVal), 0))
			} else {
				t.SetProgress("no files found")
			}
			return nil
		}
		err = g.Error(err, "could not read from file")
		return
	}
	defer t.df.Close()

	// set schema if needed
	t.Config.Target.Object = setSchema(cast.ToString(t.Config.Target.Data["schema"]), t.Config.Target.Object)
	t.Config.Target.Options.TableTmp = setSchema(cast.ToString(t.Config.Target.Data["schema"]), t.Config.Target.Options.TableTmp)

	t.SetProgress("writing to target database [mode: %s]", t.Config.Mode)
	defer t.Cleanup()
	cnt, err := t.WriteToDb(t.Config, t.df, tgtConn)
	if err != nil {
		err = g.Error(err, "could not write to database")
		if t.Config.Target.TmpTableCreated {
			// need to drop residue
			tgtConn.DropTable(t.Config.Target.Options.TableTmp)
		}
		return
	}

	elapsed := int(time.Since(start).Seconds())
	t.SetProgress("inserted %d rows in %d secs [%s r/s]", cnt, elapsed, getRate(cnt))

	if err != nil {
		err = g.Error(t.df.Err(), "error in transfer")
	}
	return
}

func (t *TaskExecution) runFileToFile() (err error) {

	start = time.Now()

	if t.Config.Options.StdIn {
		t.SetProgress("reading from stream (stdin)")
	} else {
		t.SetProgress("reading from source file system (%s)", t.Config.SrcConn.Type)
	}
	t.df, err = t.ReadFromFile(t.Config)
	if err != nil {
		if strings.Contains(err.Error(), "Provided 0 files") {
			if t.usingCheckpoint() && t.Config.IncrementalVal != "" {
				t.SetProgress("no new files found since latest timestamp (%s)", time.Unix(cast.ToInt64(t.Config.IncrementalVal), 0))
			} else {
				t.SetProgress("no files found")
			}
			return nil
		}
		err = g.Error(err, "Could not ReadFromFile")
		return
	}
	defer t.df.Close()

	if t.Config.Options.StdOut {
		t.SetProgress("writing to target stream (stdout)")
	} else {
		t.SetProgress("writing to target file system (%s)", t.Config.TgtConn.Type)
	}
	defer t.Cleanup()
	cnt, err := t.WriteToFile(t.Config, t.df)
	if err != nil {
		err = g.Error(err, "Could not WriteToFile")
		return
	}

	t.SetProgress("wrote %d rows [%s r/s]", cnt, getRate(cnt))

	if t.df.Err() != nil {
		err = g.Error(t.df.Err(), "Error in runFileToFile")
	}
	return
}

func (t *TaskExecution) runDbToDb() (err error) {
	start = time.Now()
	if t.Config.Mode == Mode("") {
		t.Config.Mode = AppendMode
	}

	// Initiate connections
	srcConn, err := t.getSrcDBConn()
	if err != nil {
		err = g.Error(err, "Could not initialize source connection")
		return
	}

	tgtProps := g.MapToKVArr(t.Config.TgtConn.DataS())
	tgtConn, err := database.NewConnContext(t.Context.Ctx, t.Config.TgtConn.URL(), tgtProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	t.SetProgress("connecting to source database (%s)", srcConn.GetType())
	err = srcConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Config.SrcConn.Info().Name, srcConn.GetType())
		return
	}

	t.SetProgress("connecting to target database (%s)", tgtConn.GetType())
	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Config.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer srcConn.Close()
	defer tgtConn.Close()

	defer func() {
		if err != nil {
			// g.Trace(strings.Join(srcConn.Base().Log, ";\n"))
			// g.Trace(strings.Join(tgtConn.Base().Log, ";\n"))
		}
	}()

	// set schema if needed
	t.Config.Target.Object = setSchema(cast.ToString(t.Config.Target.Data["schema"]), t.Config.Target.Object)
	t.Config.Target.Options.TableTmp = setSchema(cast.ToString(t.Config.Target.Data["schema"]), t.Config.Target.Options.TableTmp)

	// check if table exists by getting target columns
	t.Config.Target.columns, _ = tgtConn.GetSQLColumns("select * from " + t.Config.Target.Object)

	// get watermark
	if t.usingCheckpoint() {
		t.SetProgress("getting checkpoint value")
		t.Config.IncrementalVal, err = getIncrementalValue(t.Config, tgtConn, srcConn.Template().Variable)
		if err != nil {
			err = g.Error(err, "Could not get incremental value")
			return err
		}
	}

	t.SetProgress("reading from source database")
	t.df, err = t.ReadFromDB(t.Config, srcConn)
	if err != nil {
		err = g.Error(err, "Could not ReadFromDB")
		return
	}
	defer t.df.Close()

	// to DirectLoad if possible
	if t.df.FsURL != "" {
		data := g.M("url", t.df.FsURL)
		for k, v := range srcConn.Props() {
			data[k] = v
		}
		t.Config.Source.Data["SOURCE_FILE"] = g.M("data", data)
	}

	t.SetProgress("writing to target database [mode: %s]", t.Config.Mode)
	defer t.Cleanup()
	cnt, err := t.WriteToDb(t.Config, t.df, tgtConn)
	if err != nil {
		err = g.Error(err, "Could not WriteToDb")
		return
	}

	bytesStr := ""
	if val := t.GetBytesString(); val != "" {
		bytesStr = "[" + val + "]"
	}
	elapsed := int(time.Since(start).Seconds())
	t.SetProgress("inserted %d rows in %d secs [%s r/s] %s", cnt, elapsed, getRate(cnt), bytesStr)

	if t.df.Context.Err() != nil {
		err = g.Error(t.df.Context.Err(), "Error running runDbToDb")
	}
	return
}

// ReadFromDB reads from a source database
func (t *TaskExecution) ReadFromDB(cfg *Config, srcConn database.Connection) (df *iop.Dataflow, err error) {
	srcTable := ""
	sql := ""
	fieldsStr := "*"
	streamNameArr := regexp.MustCompile(`\s`).Split(cfg.Source.Stream, -1)
	if len(streamNameArr) == 1 && !strings.Contains(cfg.Source.Stream, "/") { // has no whitespace or "/", is a table/view
		srcTable = streamNameArr[0]
		srcTable = setSchema(cast.ToString(cfg.Source.Data["schema"]), srcTable)
		if len(cfg.Source.Columns) > 0 {
			fields := lo.Map(cfg.Source.Columns, func(f string, i int) string {
				return srcConn.Quote(f)
			})
			fieldsStr = strings.Join(fields, ", ")
		}
		sql = g.F(`select %s from %s`, fieldsStr, srcTable)
	} else {
		sql = cfg.Source.Stream
	}

	// check if referring to a SQL file
	if strings.HasSuffix(strings.ToLower(cfg.Source.Stream), ".sql") {
		// for incremental, need to put `{incremental_where_cond}` for proper selecting
		sqlFromFile, err := getSQLText(cfg.Source.Stream)
		if err != nil {
			err = g.Error(err, "Could not get getSQLText for: "+cfg.Source.Stream)
			if srcTable == "" {
				return t.df, err
			} else {
				err = nil // don't return error in case the table full name ends with .sql
				g.LogError(err)
			}
		} else {
			sql = sqlFromFile
		}
	}

	// Get source columns
	cfg.Source.columns, err = srcConn.GetSQLColumns(g.R(sql, "incremental_where_cond", "1=0"))
	if err != nil {
		err = g.Error(err, "Could not obtain source columns")
		return t.df, err
	}

	// if cfg.Mode == IncrementalMode || (cfg.Mode == AppendMode && cfg.Source.UpdateKey != "") {
	if t.usingCheckpoint() {
		// select only records that have been modified after last max value
		incrementalWhereCond := "1=1"
		if cfg.IncrementalVal != "" {
			greaterThan := ">="
			if val := os.Getenv("SLING_GREATER_THAN_EQUAL"); val != "" {
				greaterThan = lo.Ternary(cast.ToBool(val), ">=", ">")
			}
			incrementalWhereCond = g.R(
				"{update_key} {gt} {value}",
				"update_key", srcConn.Quote(cfg.Source.columns.Normalize(cfg.Source.UpdateKey)),
				"value", cfg.IncrementalVal,
				"gt", greaterThan,
			)
		}

		if srcTable != "" {
			sql = g.R(
				`select {fields} from {table} where {incremental_where_cond}`,
				"fields", fieldsStr,
				"table", srcTable,
				"incremental_where_cond", incrementalWhereCond,
			)
		} else {
			if !strings.Contains(sql, "{incremental_where_cond}") {
				err = g.Error("For incremental loading with custom SQL, need to include where clause placeholder {incremental_where_cond}. e.g: select * from my_table where col2='A' AND {incremental_where_cond}")
				return t.df, err
			}
			sql = g.R(sql, "incremental_where_cond", incrementalWhereCond)
		}
	} else if cfg.Source.Limit > 0 && srcTable != "" {
		sql = g.R(
			srcConn.Template().Core["limit"],
			"fields", fieldsStr,
			"table", srcTable,
			"limit", cast.ToString(cfg.Source.Limit),
		)
	}

	df, err = srcConn.BulkExportFlow(sql)
	if err != nil {
		err = g.Error(err, "Could not BulkStream: "+sql)
		return t.df, err
	}

	return
}

// ReadFromAPI reads from a source API
func (t *TaskExecution) ReadFromAPI(cfg *Config, client *airbyte.Airbyte) (df *iop.Dataflow, err error) {

	df = iop.NewDataflow()
	var stream *iop.Datastream

	if cfg.SrcConn.Type.IsAirbyte() {
		config := airbyte.StreamConfig{
			Columns:   cfg.Source.Columns,
			StartDate: cfg.IncrementalVal,
		}
		stream, err = client.Stream(cfg.Source.Stream, config)
		if err != nil {
			err = g.Error(err, "Could not read stream '%s' for connection: %s", cfg.Source.Stream, cfg.SrcConn.Type)
			return t.df, err
		}

		df, err = iop.MakeDataFlow(stream)
		if err != nil {
			err = g.Error(err, "Could not MakeDataFlow")
			return t.df, err
		}
	} else {
		err = g.Error("API type not implemented: %s", cfg.SrcConn.Type)
	}

	return
}

// ReadFromFile reads from a source file
func (t *TaskExecution) ReadFromFile(cfg *Config) (df *iop.Dataflow, err error) {

	var stream *iop.Datastream

	if cfg.SrcConn.URL() != "" {
		// construct props by merging with options
		options := g.M()
		g.Unmarshal(g.Marshal(cfg.Source.Options), &options)
		options["METADATA"] = g.Marshal(t.getMetadata())
		options["SLING_FS_TIMESTAMP"] = t.Config.IncrementalVal
		props := append(
			g.MapToKVArr(cfg.SrcConn.DataS()),
			g.MapToKVArr(g.ToMapString(options))...,
		)

		fs, err := filesys.NewFileSysClientFromURLContext(t.Context.Ctx, cfg.SrcConn.URL(), props...)
		if err != nil {
			err = g.Error(err, "Could not obtain client for %s ", cfg.SrcConn.Type)
			return t.df, err
		}

		fsCfg := filesys.FileStreamConfig{Columns: cfg.Source.Columns, Limit: cfg.Source.Limit}
		df, err = fs.ReadDataflow(cfg.SrcConn.URL(), fsCfg)
		if err != nil {
			err = g.Error(err, "Could not FileSysReadDataflow for %s", cfg.SrcConn.Type)
			return t.df, err
		}
	} else {
		stream, err = filesys.MakeDatastream(bufio.NewReader(os.Stdin))
		if err != nil {
			err = g.Error(err, "Could not MakeDatastream")
			return t.df, err
		}
		df, err = iop.MakeDataFlow(stream.Split()...)
		if err != nil {
			err = g.Error(err, "Could not MakeDataFlow for Stdin")
			return t.df, err
		}
	}

	if len(df.Columns) == 0 {
		err = g.Error("Could not read columns")
		return df, err
	}

	return
}

// WriteToFile writes to a target file
func (t *TaskExecution) WriteToFile(cfg *Config, df *iop.Dataflow) (cnt uint64, err error) {
	var stream *iop.Datastream
	var bw int64
	defer t.PBar.Finish()

	if cfg.TgtConn.URL() != "" {
		dateMap := iop.GetISO8601DateMap(time.Now())
		cfg.TgtConn.Set(g.M("url", g.Rm(cfg.TgtConn.URL(), dateMap)))

		// construct props by merging with options
		options := g.M()
		g.Unmarshal(g.Marshal(cfg.Target.Options), &options)
		props := append(
			g.MapToKVArr(cfg.TgtConn.DataS()),
			g.MapToKVArr(g.ToMapString(options))...,
		)

		fs, err := filesys.NewFileSysClientFromURLContext(t.Context.Ctx, cfg.TgtConn.URL(), props...)
		if err != nil {
			err = g.Error(err, "Could not obtain client for: %s", cfg.TgtConn.Type)
			return cnt, err
		}

		bw, err = fs.WriteDataflow(df, cfg.TgtConn.URL())
		if err != nil {
			err = g.Error(err, "Could not FileSysWriteDataflow")
			return cnt, err
		}
		cnt = df.Count()
	} else if cfg.Options.StdOut {
		stream = iop.MergeDataflow(df)
		stream.SetConfig(map[string]string{"delimiter": ","})
		reader := stream.NewCsvReader(0, 0)
		bufStdout := bufio.NewWriter(os.Stdout)
		defer bufStdout.Flush()
		bw, err = filesys.Write(reader, bufStdout)
		if err != nil {
			err = g.Error(err, "Could not write to Stdout")
			return
		} else if err = stream.Context.Err(); err != nil {
			err = g.Error(err, "encountered stream error")
			return
		}
		cnt = stream.Count
	} else {
		err = g.Error("target for output is not specified")
		return
	}

	g.Debug(
		"wrote %s: %d rows [%s r/s]",
		humanize.Bytes(cast.ToUint64(bw)), cnt, getRate(cnt),
	)

	return
}

// WriteToDb writes to a target DB
// create temp table
// load into temp table
// insert / incremental / replace into target table
func (t *TaskExecution) WriteToDb(cfg *Config, df *iop.Dataflow, tgtConn database.Connection) (cnt uint64, err error) {
	targetTable := cfg.Target.Object
	defer t.PBar.Finish()

	if cfg.Target.Options.TableTmp == "" {
		cfg.Target.Options.TableTmp = targetTable
		if g.In(tgtConn.GetType(), dbio.TypeDbOracle) {
			if len(cfg.Target.Options.TableTmp) > 24 {
				cfg.Target.Options.TableTmp = cfg.Target.Options.TableTmp[:24] // max is 30 chars
			}
			// some weird column / commit error, not picking up latest columns
			cfg.Target.Options.TableTmp = cfg.Target.Options.TableTmp + "_tmp" + g.RandString(g.NumericRunes, 1) + strings.ToLower(g.RandString(g.AplhanumericRunes, 1))
		} else {
			cfg.Target.Options.TableTmp = cfg.Target.Options.TableTmp + "_tmp"
		}
	}
	if cfg.Mode == "" {
		cfg.Mode = AppendMode
	}

	// pre SQL
	if cfg.Target.Options.PreSQL != "" {
		t.SetProgress("executing pre-sql")
		sql, err := getSQLText(cfg.Target.Options.PreSQL)
		if err != nil {
			err = g.Error(err, "could not get pre-sql body")
			return cnt, err
		}
		_, err = tgtConn.Exec(sql)
		if err != nil {
			err = g.Error(err, "could not execute pre-sql on target")
			return cnt, err
		}
	}

	// Drop & Create the temp table
	err = tgtConn.DropTable(cfg.Target.Options.TableTmp)
	if err != nil {
		err = g.Error(err, "could not drop table "+cfg.Target.Options.TableTmp)
		return
	}
	sampleData := iop.NewDataset(df.Columns)
	sampleData.Rows = df.Buffer
	sampleData.SafeInference = true
	_, err = createTableIfNotExists(tgtConn, sampleData, cfg.Target.Options.TableTmp, "")
	if err != nil {
		err = g.Error(err, "could not create temp table "+cfg.Target.Options.TableTmp)
		return
	}
	cfg.Target.TmpTableCreated = true
	t.AddCleanupTask(func() {
		err := tgtConn.DropTable(cfg.Target.Options.TableTmp)
		g.LogError(err)
	})

	err = tgtConn.BeginContext(df.Context.Ctx)
	if err != nil {
		err = g.Error(err, "could not open transcation to write to temp table")
		return
	}

	t.SetProgress("streaming data")
	cnt, err = tgtConn.BulkImportFlow(cfg.Target.Options.TableTmp, df)
	if err != nil {
		tgtConn.Rollback()
		if os.Getenv("SLING_CLI") == "TRUE" && (cfg.SrcConn.Type.IsFile() || cfg.Options.StdIn) {
			err = g.Error(err, "could not insert into %s. Maybe try a higher sample size (SAMPLE_SIZE=2000)?", targetTable)
		} else {
			err = g.Error(err, "could not insert into "+targetTable)
		}
		return
	}

	tgtConn.Commit()
	t.PBar.Finish()

	tCnt, _ := tgtConn.GetCount(cfg.Target.Options.TableTmp)
	if cnt != tCnt {
		err = g.Error("inserted in temp table but table count (%d) != stream count (%d). Records missing. Aborting", tCnt, cnt)
		return
	}
	// aggregate stats from stream processors
	df.SyncStats()

	// Checksum Comparison, data quality. Limit to 10k, cause sums get too high
	if df.Count() <= 10000 {
		err = tgtConn.CompareChecksums(cfg.Target.Options.TableTmp, df.Columns)
		if err != nil {
			if os.Getenv("ERROR_ON_CHECKSUM_FAILURE") != "" {
				return
			}
			if g.IsDebugLow() {
				g.Debug(g.ErrMsgSimple(err))
			}
		}
	}

	// need to contain the final write in a transcation after data is loaded
	txOptions := sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: false}
	switch tgtConn.GetType() {
	case dbio.TypeDbSnowflake:
		txOptions = sql.TxOptions{}
	}
	err = tgtConn.BeginContext(df.Context.Ctx, &txOptions)
	if err != nil {
		err = g.Error(err, "could not open transcation to write to final table")
		return
	}

	defer tgtConn.Rollback() // rollback in case of error

	if cnt > 0 {
		if cfg.Mode == FullRefreshMode {
			// drop, (create if not exists) and insert directly
			err = tgtConn.DropTable(targetTable)
			if err != nil {
				err = g.Error(err, "could not drop table "+targetTable)
				return cnt, err
			}
			t.SetProgress("dropped table " + targetTable)
		}

		// create table if not exists
		sample := iop.NewDataset(df.Columns)
		sample.Rows = df.Buffer
		sample.Inferred = true // already inferred with SyncStats
		created, err := createTableIfNotExists(
			tgtConn,
			sample,
			targetTable,
			cfg.Target.Options.TableDDL,
		)
		if err != nil {
			err = g.Error(err, "could not create table "+targetTable)
			return cnt, err
		} else if created {
			t.SetProgress("created table %s", targetTable)
		}

		// TODO: put corrective action here, based on StreamProcessor's result
		// --> use StreamStats to generate new DDL, create the target
		// table with the new DDL, then insert into.
		// IF! Target table exists, and the DDL is insufficient, then
		// have setting called PermitTableSchemaOptimization, which
		// allows sling elt to alter the final table to fit the data
		// if table doesn't exists, then easy-peasy, create it.
		// change logic in createTableIfNotExists ot use StreamStats
		// OptimizeTable creates the table if it's missing
		// **Hole in this**: will truncate data points, since it is based
		// only on new data being inserted... would need a complete
		// stats of the target table to properly optimize.
		if !created && PermitTableSchemaOptimization {
			err = tgtConn.OptimizeTable(targetTable, sample.Columns)
			if err != nil {
				err = g.Error(err, "could not optimize table schema")
				return cnt, err
			}
			// if target table exists, add new columns (if we're not dropping)
		}
		if !created && cfg.Mode != FullRefreshMode && cfg.Target.Options.AddNewColumns {
			err = database.AddMissingColumns(tgtConn, targetTable, sample.Columns)
			if err != nil {
				err = g.Error(err, "could not add missing columns")
				return cnt, err
			}
		}
	}

	// if update-key is provided without primary-key, append incrementally
	if t.Config.Source.UpdateKey != "" && len(t.Config.Source.PrimaryKey) == 0 {
		t.Config.Mode = AppendMode
	}

	// Put data from tmp to final
	if cnt == 0 {
		t.SetProgress("0 rows inserted. Nothing to do.")
	} else if cfg.Mode == "drop (need to optimize temp table in place)" {
		// use swap
		err = tgtConn.SwapTable(cfg.Target.Options.TableTmp, targetTable)
		if err != nil {
			err = g.Error(err, "could not swap tables %s to %s", cfg.Target.Options.TableTmp, targetTable)
			return 0, err
		}

		err = tgtConn.DropTable(cfg.Target.Options.TableTmp)
		if err != nil {
			err = g.Error(err, "could not drop table "+cfg.Target.Options.TableTmp)
			return
		}
		t.SetProgress("dropped old table of " + targetTable)

	} else if cfg.Mode == AppendMode || cfg.Mode == SnapshotMode || cfg.Mode == FullRefreshMode {
		// create if not exists and insert directly
		err = insertFromTemp(cfg, tgtConn)
		if err != nil {
			err = g.Error(err, "Could not insert from temp")
			return 0, err
		}
	} else if cfg.Mode == TruncateMode {
		// truncate (create if not exists) and insert directly
		truncSQL := g.R(
			tgtConn.GetTemplateValue("core.truncate_table"),
			"table", targetTable,
		)
		_, err = tgtConn.Exec(truncSQL)
		if err != nil {
			err = g.Error(err, "Could not truncate table: "+targetTable)
			return
		}
		t.SetProgress("truncated table " + targetTable)

		// insert
		err = insertFromTemp(cfg, tgtConn)
		if err != nil {
			err = g.Error(err, "Could not insert from temp")
			// data is still in temp table at this point
			// need to decide whether to drop or keep it for future use
			return 0, err
		}
	} else if cfg.Mode == IncrementalMode {
		// insert in temp
		// create final if not exists
		// delete from final and insert
		// or update (such as merge or ON CONFLICT)
		rowAffCnt, err := tgtConn.Upsert(cfg.Target.Options.TableTmp, targetTable, cfg.Source.PrimaryKey)
		if err != nil {
			err = g.Error(err, "Could not incremental from temp")
			// data is still in temp table at this point
			// need to decide whether to drop or keep it for future use
			return 0, err
		}
		g.Debug("%d TOTAL INSERTS / UPDATES", rowAffCnt)
	}

	// post SQL
	if postSQL := cfg.Target.Options.PostSQL; postSQL != "" {
		t.SetProgress("executing post-sql")
		if strings.HasSuffix(strings.ToLower(postSQL), ".sql") {
			postSQL, err = getSQLText(cfg.Target.Options.PostSQL)
			if err != nil {
				err = g.Error(err, "Error executing Target.PostSQL. Could not get getSQLText for: "+cfg.Target.Options.PostSQL)
				return cnt, err
			}
		}
		_, err = tgtConn.Exec(postSQL)
		if err != nil {
			err = g.Error(err, "Error executing Target.PostSQL")
			return cnt, err
		}
	}

	err = tgtConn.Commit()
	if err != nil {
		err = g.Error(err, "could not commit")
		return cnt, err
	}

	err = df.Context.Err()
	return
}

func (t *TaskExecution) AddCleanupTask(f func()) {
	t.Context.Mux.Lock()
	defer t.Context.Mux.Unlock()
	t.cleanupFuncs = append(t.cleanupFuncs, f)
}

func (t *TaskExecution) Cleanup() {
	t.Context.Mux.Lock()
	defer t.Context.Mux.Unlock()

	for i, f := range t.cleanupFuncs {
		f()
		t.cleanupFuncs[i] = func() {} // in case it gets called again
	}
	if t.df != nil {
		t.df.CleanUp()
	}
}

func (t *TaskExecution) usingCheckpoint() bool {
	return t.Config.Source.UpdateKey != "" && (t.Config.Mode == IncrementalMode || t.Config.Mode == AppendMode)
}

func createTableIfNotExists(conn database.Connection, data iop.Dataset, tableName string, tableDDL string) (created bool, err error) {

	// check table existence
	exists, err := conn.TableExists(tableName)
	if err != nil {
		return false, g.Error(err, "Error checking table "+tableName)
	} else if exists {
		return false, nil
	}

	if tableDDL == "" {
		tableDDL, err = conn.GenerateDDL(tableName, data, false)
		if err != nil {
			return false, g.Error(err, "Could not generate DDL for "+tableName)
		}
	}

	_, err = conn.Exec(tableDDL)
	if err != nil {
		errorFilterTableExists := conn.GetTemplateValue("variable.error_filter_table_exists")
		if errorFilterTableExists != "" && strings.Contains(err.Error(), errorFilterTableExists) {
			return false, g.Error(err, "Error creating table %s as it already exists", tableName)
		}
		return false, g.Error(err, "Error creating table "+tableName)
	}

	return true, nil
}

func insertFromTemp(cfg *Config, tgtConn database.Connection) (err error) {
	// insert
	tmpColumns, err := tgtConn.GetColumns(cfg.Target.Options.TableTmp)
	if err != nil {
		err = g.Error(err, "could not get column list for "+cfg.Target.Options.TableTmp)
		return
	}
	tgtColumns, err := tgtConn.GetColumns(cfg.Target.Object)
	if err != nil {
		err = g.Error(err, "could not get column list for "+cfg.Target.Object)
		return
	}

	// if tmpColumns are dummy fields, simply match the target column names
	if iop.IsDummy(tmpColumns) && len(tmpColumns) == len(tgtColumns) {
		for i, col := range tgtColumns {
			tmpColumns[i].Name = col.Name
		}
	}

	// TODO: need to validate the source table types are casted
	// into the target column type
	tgtFields, err := tgtConn.ValidateColumnNames(
		tgtColumns.Names(),
		tmpColumns.Names(),
		true,
	)
	if err != nil {
		err = g.Error(err, "columns mismatched")
		return
	}

	srcFields := tgtConn.CastColumnsForSelect(tmpColumns, tgtColumns)

	sql := g.R(
		tgtConn.Template().Core["insert_from_table"],
		"tgt_table", cfg.Target.Object,
		"src_table", cfg.Target.Options.TableTmp,
		"tgt_fields", strings.Join(tgtFields, ", "),
		"src_fields", strings.Join(srcFields, ", "),
	)
	_, err = tgtConn.Exec(sql)
	if err != nil {
		err = g.Error(err, "Could not execute SQL: "+sql)
		return
	}
	g.Debug("inserted rows into `%s` from temp table `%s`", cfg.Target.Object, cfg.Target.Options.TableTmp)
	return
}

func getIncrementalValue(cfg *Config, tgtConn database.Connection, srcConnVarMap map[string]string) (val string, err error) {
	// get table columns type for table creation if not exists
	// in order to get max value
	// does table exists?
	// get max value from key_field
	sql := g.F(
		"select max(%s) as max_val from %s",
		tgtConn.Quote(cfg.Source.UpdateKey),
		cfg.Target.Object,
	)

	data, err := tgtConn.Query(sql)
	if err != nil {
		if strings.Contains(err.Error(), "exist") {
			// table does not exists, will be create later
			// set val to blank for full load
			return "", nil
		}
		err = g.Error(err, "could not get max value for "+cfg.Source.UpdateKey)
		return
	}
	if len(data.Rows) == 0 {
		// table is empty
		// set val to blank for full load
		return "", nil
	}

	value := data.Rows[0][0]
	colType := data.Columns[0].Type
	if colType.IsDatetime() {
		val = g.R(
			srcConnVarMap["timestamp_layout_str"],
			"value", cast.ToTime(value).Format(srcConnVarMap["timestamp_layout"]),
		)
	} else if colType == iop.DateType {
		val = g.R(
			srcConnVarMap["date_layout_str"],
			"value", cast.ToTime(value).Format(srcConnVarMap["date_layout"]),
		)
	} else if colType.IsNumber() {
		val = cast.ToString(value)
	} else {
		val = strings.ReplaceAll(cast.ToString(value), `'`, `''`)
		val = `'` + val + `'`
	}

	return
}

func getRate(cnt uint64) string {
	return humanize.Commaf(math.Round(cast.ToFloat64(cnt) / time.Since(start).Seconds()))
}

// getSQLText process source sql file / text
func getSQLText(filePath string) (string, error) {
	filePath = strings.TrimPrefix(filePath, "file://")
	_, err := os.Stat(filePath)
	if err != nil {
		return "", g.Error(err, "Could not find file -> "+filePath)
	}
	bytes, err := ioutil.ReadFile(filePath)
	if err != nil {
		return "", g.Error(err, "Could not ReadFile: "+filePath)
	}

	return string(bytes), nil
}
