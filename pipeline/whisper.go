/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#
# ***** END LICENSE BLOCK *****/

package pipeline

import (
	"fmt"
	"github.com/rafrombrc/whisper-go/whisper"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
)

// WhisperRunners listen for *whisper.Point data values to come in on an input
// channel and write the values out to a single whisper db file as they do.
type WhisperRunner interface {
	InChan() chan *whisper.Point
}

type wRunner struct {
	path   string
	db     *whisper.Whisper
	inChan chan *whisper.Point
	wg     *sync.WaitGroup
}

func NewWhisperRunner(path_ string, archiveInfo []whisper.ArchiveInfo,
	aggMethod whisper.AggregationMethod, wg *sync.WaitGroup) (
	wr WhisperRunner, err error) {

	var db *whisper.Whisper
	if db, err = whisper.Open(path_); err != nil {
		if !os.IsNotExist(err) {
			// A real error.
			err = fmt.Errorf("Error opening whisper db: %s", err)
			return
		}

		// First make sure the folder is there.
		dir := path.Dir(path_)
		if _, err = os.Stat(dir); os.IsNotExist(err) {
			if err = os.MkdirAll(dir, 0700); err != nil {
				err = fmt.Errorf("Error creating whisper db folder '%s': %s", dir, err)
				return
			}
		} else if err != nil {
			err = fmt.Errorf("Error opening whisper db folder '%s': %s", dir, err)
		}
		if db, err = whisper.Create(path_, archiveInfo, 0.1, aggMethod, false); err != nil {
			err = fmt.Errorf("Error creating whisper db: %s", err)
			return
		}
	}
	inChan := make(chan *whisper.Point, 10)
	realWr := &wRunner{path_, db, inChan, wg}
	realWr.start()
	wr = realWr
	return
}

func (wr *wRunner) start() {
	go func() {
		var err error
		for point := range wr.InChan() {
			if err = wr.db.Update(*point); err != nil {
				log.Printf("Error updating whisper db '%s': %s", wr.path, err)
			}
		}
		wr.wg.Done()
	}()
}

func (wr *wRunner) InChan() chan *whisper.Point {
	return wr.inChan
}

// A WhisperOutput plugin will parse the stats data in the payload of a
// `statmetric` message and write the data out to a graphite-compatible
// whisper database file tree structure.
type WhisperOutput struct {
	basePath           string
	defaultAggMethod   whisper.AggregationMethod
	defaultArchiveInfo []whisper.ArchiveInfo
	dbs                map[string]WhisperRunner
}

type WhisperOutputConfig struct {
	// Full file path to where the Whisper db files are stored.
	BasePath string

	// Default mechanism whisper will use to aggregate data points as they
	// roll from more precise (i.e. more recent) to less precise storage.
	DefaultAggMethod whisper.AggregationMethod

	// Slice of 3-tuples, each 3-tuple describes a time interval's storage policy:
	// [<# of secs per datapoint> <# of datapoints> <# of secs retention>]
	DefaultArchiveInfo [][3]uint32
}

func (o *WhisperOutput) ConfigStruct() interface{} {
	basePath := path.Join("var", "run", "hekad", "whisper")

	// 60 seconds per datapoint, 1440 datapoints = 1 day of retention
	// 15 minutes per datapoint, 8 datapoints = 2 hours of retention
	// 1 hour per datapoint, 7 days of retention
	// 12 hours per datapoint, 2 years of retention
	defaultArchiveInfo := [][3]uint32{
		{0, 60, 1440}, {0, 900, 8}, {0, 3600, 168}, {0, 43200, 1456},
	}

	return &WhisperOutputConfig{
		BasePath:           basePath,
		DefaultAggMethod:   whisper.AGGREGATION_AVERAGE,
		DefaultArchiveInfo: defaultArchiveInfo,
	}
}

func (o *WhisperOutput) Init(config interface{}) (err error) {
	conf := config.(*WhisperOutputConfig)
	o.basePath = conf.BasePath
	o.defaultAggMethod = conf.DefaultAggMethod
	o.defaultArchiveInfo = make([]whisper.ArchiveInfo, len(conf.DefaultArchiveInfo))
	for i, aiSpec := range conf.DefaultArchiveInfo {
		o.defaultArchiveInfo[i] = whisper.ArchiveInfo{aiSpec[0], aiSpec[1], aiSpec[2]}
	}
	o.dbs = make(map[string]WhisperRunner)
	return
}

func (o *WhisperOutput) getFsPath(statName string) (statPath string) {
	statPath = strings.Replace(statName, ".", string(os.PathSeparator), -1)
	statPath = strings.Join([]string{statPath, "wsp"}, ".")
	statPath = path.Join(o.basePath, statPath)
	return
}

func (o *WhisperOutput) Run(or OutputRunner, h PluginHelper) (err error) {

	var (
		fields   []string
		wr       WhisperRunner
		unixTime uint64
		value    float64
		payload  string
		e        error
		pack     *PipelinePack
		wg       sync.WaitGroup
	)

	for plc := range or.InChan() {
		pack = plc.Pack
		payload = pack.Message.GetPayload()
		pack.Recycle() // Once we've copied the payload we're done w/ the pack.
		lines := strings.Split(strings.Trim(payload, " \n"), "\n")
		for _, line := range lines {
			// `fields` should be "<name> <value> <timestamp>"
			fields = strings.Fields(line)
			if len(fields) != 3 || !strings.HasPrefix(fields[0], "stats") {
				or.LogError(fmt.Errorf("malformed statmetric line: '%s'", line))
				continue
			}
			if wr = o.dbs[fields[0]]; wr == nil {
				wg.Add(1)
				wr, e = NewWhisperRunner(o.getFsPath(fields[0]), o.defaultArchiveInfo,
					o.defaultAggMethod, &wg)
				if e != nil {
					or.LogError(fmt.Errorf("can't create WhisperRunner: %s", e))
					continue
				}
				o.dbs[fields[0]] = wr
			}
			if unixTime, e = strconv.ParseUint(fields[2], 0, 32); e != nil {
				or.LogError(fmt.Errorf("parsing time: %s", e))
				continue
			}
			if value, e = strconv.ParseFloat(fields[1], 64); e != nil {
				or.LogError(fmt.Errorf("parsing value '%s': %s", fields[1], e))
				continue
			}
			pt := &whisper.Point{
				Timestamp: uint32(unixTime),
				Value:     value,
			}
			wr.InChan() <- pt
		}
	}

	for _, wr := range o.dbs {
		close(wr.InChan())
	}
	wg.Wait()

	return
}
