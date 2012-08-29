package usvisa

import (
  "appengine"
  "appengine/memcache"
  "appengine/urlfetch"
  "bytes"
  "compress/zlib"
  "fmt"
  "io/ioutil"
  "net/http"
  "regexp"
  "strconv"
  "strings"
  "text/template"
  "time"
  "errors" )

func init() {
  http.HandleFunc("/batch/", Batch)
  http.HandleFunc("/action/refresh/", Refresh)
  http.HandleFunc("/action/print/", Print)
}

const (
  PdfUrl = "http://photos.state.gov/libraries/unitedkingdom/164203/cons-visa/admin_processing_dates.pdf"
)

type BatchUpdate struct {
  Status, Date string
}

type BatchTable map[string][]BatchUpdate

var (
  BTETRE = regexp.MustCompile(`(?ms)BT\\r\\n(.+?)ET\\r\\n`)
  TextRE = regexp.MustCompile(`\\((.+?)\\)`)
)

const (
  StreamStartMarker = "stream\x0D\x0A"
  StreamEndMarker   = "endstream\x0D\x0A"
)

func LoadBatchTable(c appengine.Context, url string) (BatchTable, error) {
  c.Infof("Started downloading")
  duration, _ := time.ParseDuration("1m")
  client := &http.Client{
    Transport: &urlfetch.Transport{
      Context:                       c,
      Deadline:                      duration,
      AllowInvalidServerCertificate: true,
    },
  }
  response, err := client.Get(url)
  if err != nil {
    c.Errorf("GET failed, [%v]", err)
    return nil, errors.New("GET failed")
  }
  defer response.Body.Close()
  contents, err := ioutil.ReadAll(response.Body)
  if err != nil {
    c.Errorf("GET read failed, [%v]", err)
    return nil, errors.New("ReadAll failed")
  }
  c.Infof("Loaded %d bytes\n", len(contents))
  return parse(c, contents)
}

func parse(c appengine.Context, pdf []byte) (BatchTable, error) {
  table := make(BatchTable)
  for {
    begin := bytes.Index(pdf, []byte(StreamStartMarker))
    if begin == -1 {
      break
    }
    pdf = pdf[begin+len(StreamStartMarker):]
    end := bytes.Index(pdf, []byte(StreamStartMarker))
    if end == -1 {
      break
    }
    section := pdf[0:end]
    pdf = pdf[end+len(StreamEndMarker):]

    buf := bytes.NewBuffer(section)
    zr, err := zlib.NewReader(buf)
    if err != nil {
      c.Errorf("Unzip initialization failed, [%v]", err)
      return table, errors.New("Unzip initialization failed")
    }
    unzipped, err := ioutil.ReadAll(zr)
    if err != nil {
      c.Errorf("Unzip failed, [%v]", err)
      return table, errors.New("Unzip failed")
    }
    var records []string
    for _, group := range BTETRE.FindAllSubmatch(unzipped, -1) {
      var lines [][]byte
      for _, group := range TextRE.FindAllSubmatch(group[1], -1) {
        lines = append(lines, group[1])
      }
      records = append(records, string(bytes.Join(lines, []byte{})))
    }
    for i := 0; i < len(records)-2; i++ {
      v, err := strconv.ParseInt(records[i], 10, 64)
      if err == nil && v >= 20000000000 && v < 29000000000 {
        id := records[i]
        if _, exists := table[id]; !exists {
          table[id] = make([]BatchUpdate, 0)
        }
        table[id] = append(table[id], BatchUpdate{records[i+1], records[i+2]})
        i += 2
      }
    }
  }
  return table, nil
}

type Table struct {
  UpdateTime string
  Batches    BatchTable
}

func ReadTable(c appengine.Context, reload bool) (*Table, error) {
  var table Table
  if _, err := memcache.Gob.Get(c, "table", &table); err == memcache.ErrCacheMiss {
    c.Infof("No [table] record found in cache, [%v]", err)
    if reload {
      table = *RefreshTable(c)
    }
  } else if err != nil {
    c.Errorf("Unable to read [table] record, [%v]", err)
    return nil, errors.New("Unable to read a record")
  }
  c.Infof("Read %d records, updated at [%s]", len(table.Batches), table.UpdateTime)
  return &table, nil
}

func StoreTable(c appengine.Context, table *Table) {
  c.Infof("Storing %d records, updated at [%s]", len(table.Batches), table.UpdateTime)
  record := &memcache.Item{
    Key:    "table",
    Object: table,
  }
  if err := memcache.Gob.Set(c, record); err != nil {
    c.Errorf("Unable to store [table] record, [%v]", err)
  }
  c.Infof("Stored")
}

const (
  PrintTemplate = `
    <style>
      table { border-collapse:collapse; }
      table, th, td { border: 1px solid black; padding: .2em; }
      td { vertical-align:top; }
    </style>
    <p>
      # records: {{len .Records}}, 
      updated: {{.LastUpdateTime}},
      now: {{.Now}},
      age: {{.Age}},
      <a href="{{.PdfUrl}}">Original PDF</a>
    </p>
    <table>
     {{range $id, $updates := .Records}}
       <tr>
         <td>{{$id}}</td>
         <td>
           {{range $updates}}
             {{.Status}}, {{.Date}}
             <p/>
           {{end}}
         </td>
       </tr>
     {{end}}
    </table>
  `
)

func Print(w http.ResponseWriter, r *http.Request) {
  c := appengine.NewContext(r)
  c.Infof("> Print")
  defer c.Infof("Print finished")
  w.Header().Set("Content-Type", "text/html; charset=utf-8")

  table, err := ReadTable(c, false)
  if err != nil {
    fmt.Fprintf(w, "No data")
    return
  }

  last, _ := time.Parse(time.RFC3339, table.UpdateTime)
  now := time.Now().UTC()

  data := struct {
    PdfUrl                   string
    LastUpdateTime, Now, Age string
    Records                  BatchTable
  }{
    PdfUrl,
    last.Format(time.RFC3339),
    now.Format(time.RFC3339),
    now.Sub(last).String(),
    table.Batches,
  }

  template.Must(template.New("Data").Parse(PrintTemplate)).Execute(w, data)
}

func Refresh(w http.ResponseWriter, r *http.Request) {
  c := appengine.NewContext(r)
  c.Infof("> Refresh")
  defer c.Infof("Refresh finished")
  RefreshTable(c)
}

func RefreshTable(c appengine.Context) *Table {
  c.Infof("- Refreshing a table")
  table := new(Table)
  table.Batches, _ = LoadBatchTable(c, PdfUrl)
  table.UpdateTime = time.Now().UTC().Format(time.RFC3339)
  c.Infof("Storing a table to memcache")
  StoreTable(c, table)
  return table
}

func GetBatch(c appengine.Context, id string) []BatchUpdate {
  table, err := ReadTable(c, true)
  if err != nil {
    return nil
  }

  gap, _ := time.ParseDuration("1h")
  last, _ := time.Parse(time.RFC3339, table.UpdateTime)
  now := time.Now().UTC()

  c.Infof("Last updated: %s, now %s, gap %s", last.String(), now.String(), gap.String())
  check := last.Add(gap)
  c.Infof("Check time %s", check.String())

  if check.Before(now) {
    c.Infof("Time to update")
    table = RefreshTable(c)
  }

  if record, exists := table.Batches[id]; exists {
    return record
  }
  return nil
}

func Batch(w http.ResponseWriter, r *http.Request) {
  c := appengine.NewContext(r)
  w.Header().Set("Content-Type", "text/plain; charset=utf-8")

  parts := strings.Split(r.URL.Path, "/")
  id := parts[2]
  c.Infof("> Batch: [%s]", id)
  defer c.Infof("Batch done")

  updates := GetBatch(c, id)
  if updates == nil {
    fmt.Fprintf(w, "Batch not found")
  } else {
    for _, update := range updates {
      fmt.Fprintf(w, "%s\n%s\n\n", update.Status, update.Date)
    }
  }
  c.Infof("%v", updates)
}
