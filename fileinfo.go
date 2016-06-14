package filemanager

import (
	"fmt"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/mholt/caddy/caddyhttp/httpserver"
)

// FileInfo is the information about a particular file or directory
type FileInfo struct {
	IsDir    bool
	Name     string
	Size     int64
	URL      string
	ModTime  time.Time
	Mode     os.FileMode
	Path     string
	Mimetype string
	Content  string
	Type     string
}

// GetFileInfo gets the file information and, in case of error, returns the
// respective HTTP error code
func GetFileInfo(url *url.URL, c *Config) (*FileInfo, int, error) {
	var err error

	path := strings.Replace(url.Path, c.BaseURL, "", 1)
	path = filepath.Clean(path)
	path = strings.Replace(path, "\\", "/", -1)

	file := &FileInfo{Path: path}
	f, err := c.Root.Open("/" + path)
	if err != nil {
		return file, ErrorToHTTPCode(err), err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return file, ErrorToHTTPCode(err), err
	}

	file.IsDir = info.IsDir()
	file.ModTime = info.ModTime()
	file.Name = info.Name()
	file.Size = info.Size()
	file.URL = url.Path

	return file, 0, nil
}

// GetExtendedFileInfo is used to get extra parameters for FileInfo struct
func (fi *FileInfo) GetExtendedFileInfo() error {
	fi.Mimetype = mime.TypeByExtension(filepath.Ext(fi.Path))
	fi.Type = SimplifyMimeType(fi.Mimetype)

	if fi.Type == "text" {
		err := fi.Read()
		if err != nil {
			return err
		}
	}

	return nil
}

// Read is used to read a file and store its content
func (fi *FileInfo) Read() error {
	raw, err := ioutil.ReadFile(fi.Path)
	if err != nil {
		return err
	}
	fi.Content = string(raw)
	return nil
}

// HumanSize returns the size of the file as a human-readable string
// in IEC format (i.e. power of 2 or base 1024).
func (fi FileInfo) HumanSize() string {
	return humanize.IBytes(uint64(fi.Size))
}

// HumanModTime returns the modified time of the file as a human-readable string.
func (fi FileInfo) HumanModTime(format string) string {
	return fi.ModTime.Format(format)
}

// Delete handles the delete requests
func (fi FileInfo) Delete() (int, error) {
	var err error

	// If it's a directory remove all the contents inside
	if fi.IsDir {
		err = os.RemoveAll(fi.Path)
	} else {
		err = os.Remove(fi.Path)
	}

	if err != nil {
		return ErrorToHTTPCode(err), err
	}

	return http.StatusOK, nil
}

// Rename function is used tor rename a file or a directory
func (fi FileInfo) Rename(w http.ResponseWriter, r *http.Request) (int, error) {
	newname := r.Header.Get("Rename-To")
	if newname == "" {
		return http.StatusBadRequest, nil
	}

	newpath := filepath.Clean(newname)
	newpath = strings.Replace(fi.Path, fi.Name, newname, 1)

	if err := os.Rename(fi.Path, newpath); err != nil {
		return ErrorToHTTPCode(err), err
	}

	http.Redirect(w, r, strings.Replace(fi.URL, fi.Name, newname, 1), http.StatusTemporaryRedirect)
	return 0, nil
}

// ServeAsHTML is used to serve single file pages
func (fi FileInfo) ServeAsHTML(w http.ResponseWriter, r *http.Request, c *Config) (int, error) {
	if fi.IsDir {
		return fi.serveListing(w, r, c)
	}

	return fi.serveSingleFile(w, r, c)
}

func (fi FileInfo) serveSingleFile(w http.ResponseWriter, r *http.Request, c *Config) (int, error) {
	err := fi.GetExtendedFileInfo()
	if err != nil {
		return ErrorToHTTPCode(err), err
	}

	page := &Page{
		Info: &PageInfo{
			Name:   fi.Path,
			Path:   fi.Path,
			Data:   fi,
			Config: c,
		},
	}

	return page.PrintAsHTML(w, "single")
}

func (fi FileInfo) serveListing(w http.ResponseWriter, r *http.Request, c *Config) (int, error) {
	var err error

	file, err := c.Root.Open("/" + fi.Path)
	if err != nil {
		return ErrorToHTTPCode(err), err
	}
	defer file.Close()

	listing, err := fi.loadDirectoryContents(file, c)
	if err != nil {
		fmt.Println(err)
		switch {
		case os.IsPermission(err):
			return http.StatusForbidden, err
		case os.IsExist(err):
			return http.StatusGone, err
		default:
			return http.StatusInternalServerError, err
		}
	}

	listing.Context = httpserver.Context{
		Root: c.Root,
		Req:  r,
		URL:  r.URL,
	}

	// Copy the query values into the Listing struct
	var limit int
	listing.Sort, listing.Order, limit, err = handleSortOrder(w, r, c.PathScope)
	if err != nil {
		return http.StatusBadRequest, err
	}

	listing.applySort()

	if limit > 0 && limit <= len(listing.Items) {
		listing.Items = listing.Items[:limit]
		listing.ItemsLimitedTo = limit
	}

	page := &Page{
		Info: &PageInfo{
			Name:   listing.Name,
			Path:   listing.Path,
			Config: c,
			Data:   listing,
		},
	}

	return page.PrintAsHTML(w, "listing")
}

func (fi FileInfo) loadDirectoryContents(file http.File, c *Config) (*Listing, error) {
	files, err := file.Readdir(-1)
	if err != nil {
		return nil, err
	}

	listing := directoryListing(files, fi.Path)
	return &listing, nil
}

func directoryListing(files []os.FileInfo, urlPath string) Listing {
	var (
		fileinfos           []FileInfo
		dirCount, fileCount int
	)

	for _, f := range files {
		name := f.Name()

		if f.IsDir() {
			name += "/"
			dirCount++
		} else {
			fileCount++
		}

		url := url.URL{Path: "./" + name} // prepend with "./" to fix paths with ':' in the name

		fileinfos = append(fileinfos, FileInfo{
			IsDir:   f.IsDir(),
			Name:    f.Name(),
			Size:    f.Size(),
			URL:     url.String(),
			ModTime: f.ModTime().UTC(),
			Mode:    f.Mode(),
		})
	}

	return Listing{
		Name:     path.Base(urlPath),
		Path:     urlPath,
		Items:    fileinfos,
		NumDirs:  dirCount,
		NumFiles: fileCount,
	}
}

// SimplifyMimeType returns the base type of a file
func SimplifyMimeType(name string) string {
	if strings.HasPrefix(name, "video") {
		return "video"
	}

	if strings.HasPrefix(name, "audio") {
		return "audio"
	}

	if strings.HasPrefix(name, "image") {
		return "image"
	}

	return "text"
}
