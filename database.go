package sqlittle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/bits"
)

const (
	headerMagic = "SQLite format 3\x00"
	headerSize  = 100
	// CacheTables is the number of tables pages to keep in memory. Default
	// size per page is about 4K.
	CacheTables = 100
)

var (
	ErrHeaderInvalidMagic    = errors.New("invalid magic number")
	ErrHeaderInvalidPageSize = errors.New("invalid page size")
	ErrNoSuchTable           = errors.New("no such table")
	ErrNoSuchIndex           = errors.New("no such index")
	ErrCorrupted             = errors.New("database corrupted")
	ErrIncompatible          = errors.New("incompatible database version")
	ErrEncoding              = errors.New("unsupported encoding")
)

type header struct {
	// The database page size in bytes.
	PageSize int
	// Bytes of unused "reserved" space at the end of each page. Usually 0.
	ReservedSpace int
}

type Database struct {
	l      loader
	header header
	tables *tableCache
}

// OpenFile opens a .sqlite file. This is the main entry point.
// Use database.Close() when done.
func OpenFile(f string) (*Database, error) {
	l, err := newMMapLoader(f)
	if err != nil {
		return nil, err
	}
	return newDatabase(l)
}

func newDatabase(l loader) (*Database, error) {
	buf, err := l.header()
	if err != nil {
		return nil, err
	}
	header, err := parseHeader(buf)
	if err != nil {
		return nil, err
	}

	db := &Database{
		l:      l,
		header: header,
		tables: newTableCache(CacheTables),
	}
	return db, nil
}

// Close the database.
func (db *Database) Close() error {
	return db.l.Close()
}

// n starts at 1, sqlite style
func (db *Database) page(id int) ([]byte, error) {
	if id < 1 {
		return nil, errors.New("invalid page number")
	}
	return db.l.page(id, db.header.PageSize)
}

// the file header, as described in "1.2. The Database Header"
func parseHeader(b [headerSize]byte) (header, error) {
	hs := struct {
		Magic                [16]byte
		PageSize             uint16
		WriteVersion         uint8
		ReadVersion          uint8
		ReservedSpace        uint8
		MaxFraction          uint8
		MinFraction          uint8
		LeafFraction         uint8
		_                    uint32 // ChangeCounter
		_                    uint32
		_                    uint32
		_                    uint32
		_                    uint32 // SchemaCookie
		SchemaFormat         uint32
		_                    uint32
		_                    uint32
		TextEncoding         uint32
		_                    uint32
		_                    uint32
		_                    uint32
		ReservedForExpansion [20]byte
		_                    uint32
		_                    uint32
	}{}
	if err := binary.Read(bytes.NewBuffer(b[:]), binary.BigEndian, &hs); err != nil {
		return header{}, err
	}

	h := header{}

	if string(hs.Magic[:]) != headerMagic {
		return h, ErrHeaderInvalidMagic
	}

	{
		s := uint(hs.PageSize)
		if s == 1 {
			s = 1 << 16
		}
		isPower := func(n uint) bool {
			return bits.OnesCount(n) == 1
		}
		if s < 512 || s > 1<<16 || !isPower(s) {
			return header{}, ErrHeaderInvalidPageSize
		}
		h.PageSize = int(s)
	}

	if hs.ReadVersion > 2 {
		return h, ErrIncompatible
	}

	h.ReservedSpace = int(hs.ReservedSpace)

	if hs.MaxFraction != 64 ||
		hs.MinFraction != 32 ||
		hs.LeafFraction != 32 {
		return h, ErrIncompatible
	}

	// 1,2,3,4 are the only valid values. Version 1 ignores 'DESC' on indexes,
	// so we could support that as long as we ignore any 'DESC' index, but...
	switch hs.SchemaFormat {
	case 2, 3, 4:
	default:
		return h, ErrIncompatible
	}

	switch hs.TextEncoding {
	case 1:
		// UTF8. It's the only thing we currently support
	case 2, 3:
		// UTF16le and UTF16be
		return h, ErrEncoding
	default:
		return h, ErrIncompatible
	}

	for _, v := range hs.ReservedForExpansion {
		if v != 0 {
			return h, ErrIncompatible
		}
	}

	return h, nil
}

// master records are defined as:
// CREATE TABLE sqlite_master(
//     type text,
//     name text,
//     tbl_name text,
//     rootpage integer,
//     sql text
// );
type sqliteMaster struct {
	typ, name, tblName string
	rootPage           int
	sql                string
}

func (db *Database) master() ([]sqliteMaster, error) {
	master, err := db.openTable(1)
	if err != nil {
		return nil, err
	}

	var tables []sqliteMaster
	_, err = master.Iter(db, func(rowid int64, pl cellPayload) (bool, error) {
		c, err := addOverflow(db, pl)
		if err != nil {
			return false, err
		}

		e, err := parseRecord(c)
		if err != nil {
			return false, err
		}

		m := sqliteMaster{}
		if s, ok := e[0].(string); !ok {
			return false, errors.New("wrong column type for sqlite_master")
		} else {
			m.typ = s
		}
		if s, ok := e[1].(string); !ok {
			return false, errors.New("wrong column type for sqlite_master")
		} else {
			m.name = s
		}
		if s, ok := e[2].(string); !ok {
			return false, errors.New("wrong column type for sqlite_master")
		} else {
			m.tblName = s
		}
		if n, ok := e[3].(int64); !ok {
			return false, errors.New("wrong column type for sqlite_master")
		} else {
			m.rootPage = int(n)
		}
		if s, ok := e[4].(string); !ok {
			return false, errors.New("wrong column type for sqlite_master")
		} else {
			m.sql = s
		}
		tables = append(tables, m)
		return false, nil
	})
	return tables, err
}

// returns nil if the index isn't found
func (db *Database) index(name string) (indexBtree, error) {
	tables, err := db.master()
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		if t.typ == "index" && t.name == name {
			return db.openIndex(t.rootPage)
		}
	}
	return nil, nil
}

func (db *Database) openTable(page int) (tableBtree, error) {
	if p := db.tables.get(page); p != nil {
		return p, nil
	}

	buf, err := db.page(page)
	if err != nil {
		return nil, err
	}
	p, err := newTableBtree(buf, page == 1)
	if err == nil {
		db.tables.set(page, p)
	}
	return p, err
}

func (db *Database) openIndex(page int) (indexBtree, error) {
	buf, err := db.page(page)
	if err != nil {
		return nil, err
	}
	return newIndexBtree(buf)
}

// TableScanCB is the callback for TableScan(). It gets the rowid (usually an
// internal number), and the data of the row. It should return true when the
// scan should be terminated.
type TableScanCB func(int64, Record) bool

// Table opens the named table.
// Will return ErrNoSuchTable when the table isn't there (or isn't a table).
// Table pointer is always valid if err == nil.
func (db *Database) Table(name string) (*Table, error) {
	tables, err := db.master()
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		if t.typ == "table" && t.name == name {
			return &Table{db: db, root: t.rootPage}, nil
		}
	}
	return nil, ErrNoSuchTable
}

// IndexScanCB is passed to IndexScan() and IndexScanMin(). It gets the rowid
// and the values from the index. It should return true when the scan should be
// stopped.
type IndexScanCB func(int64, Record) bool

// IndexScan calls cb() for every row in the index. These will be called in the
// index order.
// The callback gets the rowid the row is about (use TableRowid() to load the
// row, if you need it), and all the columns present in the index.
// If the callback returns true (done) the scan will be stopped.
func (db *Database) IndexScan(index string, cb IndexScanCB) error {
	ind, err := db.index(index)
	if err != nil {
		return err
	}
	if ind == nil {
		return ErrNoSuchIndex
	}
	_, err = ind.Iter(
		db,
		func(pl cellPayload) (bool, error) {
			full, err := addOverflow(db, pl)
			if err != nil {
				return false, err
			}
			rec, err := parseRecord(full)
			if err != nil {
				return false, err
			}
			rowid, rec, err := chompRowid(rec)
			if err != nil {
				return false, err
			}
			return cb(rowid, rec), nil
		},
	)
	return err
}

// IndexScanMin calls cb() for every row in the index, starting from the first
// record equal or bigger then the given record. If the type of columns in the given
// record don't match those in the index a error will be returned.
// If the callback returns true (done) the scan will be stopped.
// All comments from IndexScan are valid here as well.
func (db *Database) IndexScanMin(index string, from Record, cb IndexScanCB) error {
	ind, err := db.index(index)
	if err != nil {
		return err
	}
	if ind == nil {
		return ErrNoSuchIndex
	}
	_, err = ind.IterMin(
		db,
		from,
		func(rowid int64, rec Record) (bool, error) {
			return cb(rowid, rec), nil
		},
	)
	return err
}