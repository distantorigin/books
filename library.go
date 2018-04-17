package books

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
)

var initialSchema = `create table books (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
series text,
title text not null
);
create index idx_books_title on books(title);

create table files (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
book_id integer references books(id) on delete cascade not null,
extension text not null,
original_filename text not null,
filename text not null,
file_size integer not null,
file_mtime timestamp not null,
hash text not null unique,
regexp_name text not null,
template_override text,
source text
);
create index idx_files_book_id on files(book_id);

create table authors (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
name text not null unique
);

create table books_authors (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
book_id integer not null references books(id) on delete cascade,
author_id integer not null references authors(id) on delete cascade,
unique (book_id, author_id)
);

create table tags (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
name text not null unique
);

create table files_tags (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
file_id integer not null references files(id) on delete cascade,
tag_id integer not null references tags(id) on delete cascade,
unique (file_id, tag_id)
);

create virtual table books_fts using fts4 (author, series, title, extension, tags,  filename, source);
`

func init() {
	// Add a connect hook to set synchronous = off for all connections.
	// This improves performance, especially during import,
	// but since changes aren't immediately synced to disk, data could be lost during a power outage or sudden OS crash.
	sql.Register("sqlite3async",
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				conn.Exec("pragma synchronous=off", []driver.Value{})
				return nil
			},
		})
}

// Library represents a set of books in persistent storage.
type Library struct {
	*sql.DB
	filename  string
	booksRoot string
}

// OpenLibrary opens a library stored in a file.
func OpenLibrary(filename, booksRoot string) (*Library, error) {
	db, err := sql.Open("sqlite3async", filename)
	if err != nil {
		return nil, err
	}
	return &Library{db, filename, booksRoot}, nil
}

// CreateLibrary initializes a new library in the specified file.
// Once CreateLibrary is called, the file will be ready to open and accept new books.
// Warning: This function sets up a new library for the first time. To get a Library based on an existing library file,
// call OpenLibrary.
func CreateLibrary(filename string) error {
	log.Printf("Creating library in %s\n", filename)
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return errors.Wrap(err, "Create library")
	}
	defer db.Close()

	_, err = db.Exec(initialSchema)
	if err != nil {
		return errors.Wrap(err, "Create library")
	}

	log.Printf("Library created in %s\n", filename)
	return nil
}

// ImportBook adds a book to a library.
// The file referred to by book.OriginalFilename will either be copied or moved to the location referred to by book.CurrentFilename, relative to the configured books root.
// The book will not be imported if another book already in the library has the same hash.
func (lib *Library) ImportBook(book Book, move bool) error {
	if len(book.Files) != 1 {
		return errors.New("Book to import must contain only one file")
	}
	bf := book.Files[0]
	tx, err := lib.Begin()
	if err != nil {
		return err
	}

	rows, err := tx.Query("select id from files where hash=?", bf.Hash)
	if err != nil {
		tx.Rollback()
		return err
	}
	if rows.Next() {
		// This book's hash is already in the library.
		var id int64
		rows.Scan(&id)
		tx.Rollback()
		return errors.Errorf("A duplicate book already exists with id %d", id)
	}

	rows.Close()
	if rows.Err() != nil {
		tx.Rollback()
		return errors.Wrapf(err, "Searching for duplicate book by hash %s", bf.Hash)
	}

	existingBookId, found, err := getBookIdByTitleAndAuthors(tx, book.Title, book.Authors)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "find existing book")
	}
	if !found {
		res, err := tx.Exec("insert into books (series, title) values(?, ?)", book.Series, book.Title)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "Insert new book")
		}
		book.Id, err = res.LastInsertId()
		if err != nil {
			return errors.Wrap(err, "sett new book ID")
		}
		for _, author := range book.Authors {
			if err := insertAuthor(tx, author, &book); err != nil {
				tx.Rollback()
				return errors.Wrapf(err, "inserting author %s", author)
			}
		}

	} else {
		book.Id = existingBookId
	}

	res, err := tx.Exec(`insert into files (book_id, extension, original_filename, filename, file_size, file_mtime, hash, regexp_name, source)
	values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		book.Id, bf.Extension, bf.OriginalFilename, bf.CurrentFilename, bf.FileSize, bf.FileMtime, bf.Hash, bf.RegexpName, bf.Source)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Inserting book file into the db")
	}

	id, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Fetching new book ID")
	}
	bf.Id = id

	for _, tag := range bf.Tags {
		if err := insertTag(tx, tag, &bf); err != nil {
			tx.Rollback()
			return errors.Wrapf(err, "inserting tag %s", tag)
		}
	}

	err = indexBookInSearch(tx, &book, !found)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "index book in search")
	}

	err = lib.moveOrCopyFile(book, move)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Moving or copying book")
	}

	tx.Commit()
	log.Printf("Imported book: %s: %s, ID = %d", strings.Join(book.Authors, " & "), book.Title, book.Id)

	return nil
}

func indexBookInSearch(tx *sql.Tx, book *Book, createNew bool) error {
	if len(book.Files) != 1 {
		return errors.New("Book to index must contain only one file")
	}
	bf := book.Files[0]
	joinedTags := strings.Join(bf.Tags, " ")
	if createNew {
		// Index book for searching.
		_, err := tx.Exec(`insert into books_fts (docid, author, series, title, extension, tags,  source)
	values (?, ?, ?, ?, ?, ?, ?)`,
			book.Id, strings.Join(book.Authors, " & "), book.Series, book.Title, bf.Extension, joinedTags, bf.Source)
		if err != nil {
			return err
		}
		return nil
	}
	rows, err := tx.Query("select docid, tags, extension, source from books_fts where docid=?", book.Id)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rows.Close()
		if rows.Err() != nil {
			return err
		}
		return errors.Errorf("Existing book %d not found in FTS", book.Id)
	}
	var id int64
	var tags, extension, source string
	err = rows.Scan(&id, &tags, &extension, &source)
	if err != nil {
		return err
	}
	rows.Close()

	_, err = tx.Exec("update books_fts set tags=?, extension=?, source=? where docid=?", tags+" "+joinedTags, extension+" "+bf.Extension, source+" "+bf.Source, id)
	if err != nil {
		return err
	}
	return nil
}

// insertAuthor inserts an author into the database.
func insertAuthor(tx *sql.Tx, author string, book *Book) error {
	var authorId int64
	row := tx.QueryRow("select id from authors where name=?", author)
	err := row.Scan(&authorId)
	if err == sql.ErrNoRows {
		// Insert the author
		res, err := tx.Exec("insert into authors (name) values(?)", author)
		if err != nil {
			return err
		}
		authorId, err = res.LastInsertId()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Author inserted, insert the link
	// For two authors in the same book with the same name, only insert one.
	if _, err := tx.Exec("insert or ignore into books_authors (book_id, author_id) values(?, ?)", book.Id, authorId); err != nil {
		return err
	}
	return nil
}

// insertTag inserts a tag into the database.
func insertTag(tx *sql.Tx, tag string, bf *BookFile) error {
	var tagId int64
	row := tx.QueryRow("select id from tags where name=?", tag)
	err := row.Scan(&tagId)
	if err == sql.ErrNoRows {
		// Insert the tag
		res, err := tx.Exec("insert into tags (name) values(?)", tag)
		if err != nil {
			return err
		}
		tagId, err = res.LastInsertId()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Tag inserted, insert the link
	// Avoid duplicate tags.
	if _, err := tx.Exec("insert or ignore into files_tags (file_id, tag_id) values(?, ?)", bf.Id, tagId); err != nil {
		return err
	}
	return nil
}

// moveOrCopyFile moves or copies a file from book.OriginalFilename to book.CurrentFilename, relative to the configured books root.
// All necessary directories to make the destination valid will be created.
func (lib *Library) moveOrCopyFile(book Book, move bool) error {
	if len(book.Files) != 1 {
		return errors.New("Book to move or copy must contain only one file")
	}
	bf := book.Files[0]
	newName := bf.CurrentFilename
	newPath := path.Join(lib.booksRoot, newName)
	err := os.MkdirAll(path.Dir(newPath), 0755)
	if err != nil {
		return err
	}

	if move {
		err = moveFile(bf.OriginalFilename, newPath)
	} else {
		err = copyFile(bf.OriginalFilename, newPath)
	}
	if err != nil {
		return err
	}

	return nil
}

// Search searches the library for books.
// By default, all fields are searched, but
// field:terms+to+search will limit to that field only.
// Fields: author, title, series, extension, tags, filename, source.
// Example: author:Stephen+King title:Shining
func (lib *Library) Search(terms string) ([]Book, error) {
	books, _, err := lib.SearchPaged(terms, 0, 0, 0)
	return books, err
}

// searchPaged implements book searching, both paged and non paged.
// Set limit to 0 to return all results.
// moreResults will be set to the number of additional results not returned, with a maximum of moreResultsLimit.
func (lib *Library) SearchPaged(terms string, offset, limit, moreResultsLimit int) (books []Book, moreResults int, err error) {
	books = []Book{}
	var query string
	args := []interface{}{terms}
	if limit == 0 {
		query = "select docid from books_fts where books_fts match ?"
	} else {
		query = "select docid from books_fts where books_fts match ? LIMIT ? OFFSET ?"
		args = append(args, limit+moreResultsLimit, offset)
	}

	rows, err := lib.Query(query, args...)
	if err != nil {
		return nil, 0, errors.Wrap(err, "Querying db for search terms")
	}

	var ids []int64
	var id int64
	for rows.Next() {
		rows.Scan(&id)
		ids = append(ids, id)
	}
	err = rows.Err()
	if err != nil {
		return nil, 0, errors.Wrap(err, "Retrieving search results from db")
	}

	if limit > 0 && len(ids) > limit {
		moreResults = len(ids) - limit
		ids = ids[:limit]
	}
	books, err = lib.GetBooksById(ids)

	return
}

// GetBooksById retrieves books from the library by their id.
func (lib *Library) GetBooksById(ids []int64) ([]Book, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	tx, err := lib.Begin()
	if err != nil {
		return nil, errors.Wrap(err, "get books by ID")
	}
	results := []Book{}

	query := "select id, series, title from books where id in (" + joinInt64s(ids, ",") + ")"
	rows, err := tx.Query(query)
	if err != nil {
		return results, errors.Wrap(err, "fetching books from database by ID")
	}

	for rows.Next() {
		book := Book{}
		if err := rows.Scan(&book.Id, &book.Series, &book.Title); err != nil {
			tx.Rollback()
			return nil, errors.Wrap(err, "scanning rows")
		}

		results = append(results, book)
	}

	if rows.Err() != nil {
		tx.Rollback()
		return nil, errors.Wrap(err, "querying books by ID")
	}
	rows.Close()

	authorMap, err := getAuthorsByBookIds(tx, ids)
	if err != nil {
		tx.Rollback()
		return nil, errors.Wrap(err, "get authors for books")
	}

	fileMap, err := getFilesByBookIds(tx, ids)
	if err != nil {
		tx.Rollback()
		return nil, errors.Wrap(err, "get files for books")
	}

	// Get authors and files
	for i, book := range results {
		results[i].Authors = authorMap[book.Id]
		results[i].Files = fileMap[book.Id]
	}
	err = tx.Commit()
	if err != nil {
		return nil, errors.Wrap(err, "get books by ID")
	}
	return results, nil
}

// getAuthorsByBookIds gets author names for each book ID.
func getAuthorsByBookIds(tx *sql.Tx, ids []int64) (map[int64][]string, error) {
	m := make(map[int64][]string)
	if len(ids) == 0 {
		return m, nil
	}

	var bookId int64
	var authorName string

	query := "SELECT ba.book_id, a.name FROM books_authors ba JOIN authors a ON ba.author_id = a.id WHERE ba.book_id IN (" + joinInt64s(ids, ",") + ") ORDER BY ba.id"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		err := rows.Scan(&bookId, &authorName)
		if err != nil {
			return nil, err
		}
		authors := m[bookId]
		m[bookId] = append(authors, authorName)
	}

	return m, nil
}

// getTagsByFileIds gets tag names for each book ID.
func getTagsByFileIds(tx *sql.Tx, ids []int64) (map[int64][]string, error) {
	tagsMap := make(map[int64][]string)
	if len(ids) == 0 {
		return nil, nil
	}

	var fileId int64
	var tag string

	query := "SELECT ft.file_id, t.name FROM files_tags ft JOIN tags t ON ft.tag_id = t.id WHERE ft.file_id IN (" + joinInt64s(ids, ",") + ") ORDER BY ft.id"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		err := rows.Scan(&fileId, &tag)
		if err != nil {
			return nil, err
		}
		tagsMap[fileId] = append(tagsMap[fileId], tag)
	}

	return tagsMap, nil
}

// getFilesByBookIds gets files for each book ID.
func getFilesByBookIds(tx *sql.Tx, ids []int64) (fileMap map[int64][]BookFile, err error) {
	if len(ids) == 0 {
		return nil, nil
	}
	fileIdMap := make(map[int64][]int64)
	fileMap = make(map[int64][]BookFile)

	query := "select id, book_id from files where book_id in (" + joinInt64s(ids, ",") + ")"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bookId, fileId int64
	for rows.Next() {
		err := rows.Scan(&fileId, &bookId)
		if err != nil {
			return nil, err
		}
		fileIdMap[bookId] = append(fileIdMap[bookId], fileId)
	}

	for bookId, fileIds := range fileIdMap {
		files, err := getFilesById(tx, fileIds)
		if err != nil {
			return nil, err
		}
		fileMap[bookId] = files
	}

	return fileMap, nil
}

// GetFilesById gets files for each ID.
func (lib *Library) GetFilesById(ids []int64) ([]BookFile, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	tx, err := lib.Begin()
	if err != nil {
		return nil, err
	}

	files, err := getFilesById(tx, ids)
	if err != nil {
		tx.Rollback()
	} else {
		tx.Commit()
	}

	return files, err
}

// GetFilesById gets files for each ID.
func getFilesById(tx *sql.Tx, ids []int64) ([]BookFile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	files := []BookFile{}
	tagMap, err := getTagsByFileIds(tx, ids)
	if err != nil {
		return nil, err
	}
	query := "select id, extension, original_filename, filename, file_size, file_mtime, hash, regexp_name, source from files where id in (" + joinInt64s(ids, ",") + ")"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		bf := BookFile{}
		err := rows.Scan(&bf.Id, &bf.Extension, &bf.OriginalFilename, &bf.CurrentFilename, &bf.FileSize, &bf.FileMtime, &bf.Hash, &bf.RegexpName, &bf.Source)
		if err != nil {
			return nil, err
		}
		bf.Tags = tagMap[bf.Id]
		files = append(files, bf)
	}
	return files, nil
}

// ConvertToEpub converts a file to epub, and caches it in LIBRARY_ROOT/cache.
// This depends on ebook-convert, which takes the original filename, and the new filename, in that order.
// the file's hash, with the extension .epub, will be the name of the cached file.
func (lib *Library) ConvertToEpub(file BookFile) error {
	filename := path.Join(lib.booksRoot, file.CurrentFilename)
	cacheDir := path.Join(path.Dir(lib.filename), "cache")
	newFile := path.Join(cacheDir, file.Hash+".epub")
	cmd := exec.Command("ebook-convert", filename, newFile)
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

// copyFile copies a file from src to dst, setting dst's modified time to that of src.
func copyFile(src, dst string) (e error) {
	fp, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, "Copy file")
	}
	defer fp.Close()

	st, err := fp.Stat()
	if err != nil {
		return errors.Wrap(err, "Copy file")
	}

	fd, err := os.Create(dst)
	if err != nil {
		return errors.Wrap(err, "Copy file")
	}
	defer func() {
		if err := fd.Close(); err != nil {
			e = errors.Wrap(err, "Copy file")
		}
		_ = os.Chtimes(dst, time.Now(), st.ModTime())
	}()

	if _, err := io.Copy(fd, fp); err != nil {
		return errors.Wrap(err, "Copy file")
	}

	log.Printf("Copied %s to %s", src, dst)

	return nil
}

// moveFile moves a file from src to dst.
// First, moveFile will attempt to rename the file,
// and if that fails, it will perform a copy and delete.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		err = copyFile(src, dst)
		if err != nil {
			return err
		}
		err = os.Remove(src)
		if err != nil {
			log.Printf("Error removing %s: %s", src, err)
			return nil
		}

		log.Printf("Moved %s to %s (copy/delete)", src, dst)
		return nil
	}

	log.Printf("Moved %s to %s", src, dst)
	return nil
}

// GetUniqueName checks to see if a file named f already exists, and if so, finds a unique name.
func GetUniqueName(f string) string {
	i := 1
	ext := path.Ext(f)
	newName := f
	_, err := os.Stat(newName)
	for err == nil {
		newName = strings.TrimSuffix(f, ext) + " (" + strconv.Itoa(i) + ")" + ext
		i++
		_, err = os.Stat(newName)
	}
	return newName
}

func getBookIdByTitleAndAuthors(tx *sql.Tx, title string, authors []string) (int64, bool, error) {
	rows, err := tx.Query("SELECT id FROM books WHERE title = ?", title)
	if err != nil {
		return 0, false, errors.Wrap(err, "get book by title")
	}

	var id int64
	ids := make([]int64, 0)
	for rows.Next() {
		err := rows.Scan(&id)
		if err != nil {
			return 0, false, errors.Wrap(err, "Get book ID from title")
		}

		ids = append(ids, id)
	}
	rows.Close()

	authorMap, err := getAuthorsByBookIds(tx, ids)
	if err != nil {
		return 0, false, errors.Wrap(err, "get authors for books")
	}

	authorsEqual := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}

		return true
	}

	for bookId, authorNames := range authorMap {
		if authorsEqual(authors, authorNames) {
			return bookId, true, nil
		}
	}

	return 0, false, nil
}

// joinInt64s is like strings.Join, but for slices of int64.
// SQLite limits the number of variables that can be passed to a bound query.
// Pass int64s directly to IN (…) as a work-around.
// query := “SELECT * FROM table WHERE id IN (“ + joinInt64s(ids, “,”) + “)”
func joinInt64s(items []int64, sep string) string {
	itemsStr := make([]string, len(items))
	for i, item := range items {
		itemsStr[i] = strconv.FormatInt(item, 10)
	}

	return strings.Join(itemsStr, sep)
}
