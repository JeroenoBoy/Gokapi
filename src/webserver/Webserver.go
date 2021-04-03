package webserver

/**
Handling of webserver and requests / uploads
*/

import (
	"Gokapi/src/configuration"
	"Gokapi/src/helper"
	"Gokapi/src/storage"
	"Gokapi/src/storage/filestructure"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Variable containing all parsed templates
var templateFolder *template.Template

// Starts the webserver on the port set in the config
func Start(staticFolderEmbedded, templateFolderEmbedded embed.FS) {
	initTemplates(templateFolderEmbedded)
	webserverDir, _ := fs.Sub(staticFolderEmbedded, "static")
	if helper.FolderExists("static") {
		fmt.Println("Found folder 'static', using local folder instead of internal static folder")
		http.Handle("/", http.FileServer(http.Dir("static")))
	} else {
		http.Handle("/", http.FileServer(http.FS(webserverDir)))
	}
	http.HandleFunc("/index", showIndex)
	http.HandleFunc("/d", showDownload)
	http.HandleFunc("/error", showError)
	http.HandleFunc("/login", showLogin)
	http.HandleFunc("/logout", doLogout)
	http.HandleFunc("/admin", showAdminMenu)
	http.HandleFunc("/upload", uploadFile)
	http.HandleFunc("/delete", deleteFile)
	http.HandleFunc("/downloadFile", downloadFile)
	http.HandleFunc("/forgotpw", forgotPassword)
	fmt.Println("Binding webserver to " + configuration.ServerSettings.Port)
	fmt.Println("Webserver can be accessed at " + configuration.ServerSettings.ServerUrl + "admin")
	log.Fatal(http.ListenAndServe(configuration.ServerSettings.Port, nil))
}

// Initialises the templateFolder variable by scanning through all the templates.
// If a folder "templates" exists in the main directory, it is used.
// Otherwise templateFolderEmbedded will be used.
func initTemplates(templateFolderEmbedded embed.FS) {
	var err error
	if helper.FolderExists("templates") {
		fmt.Println("Found folder 'templates', using local folder instead of internal template folder")
		templateFolder, err = template.ParseGlob("templates/*.tmpl")
		helper.Check(err)
	} else {
		templateFolder, err = template.ParseFS(templateFolderEmbedded, "templates/*.tmpl")
		helper.Check(err)
	}
}

// Sends a redirect HTTP output to the client. Variable url is used to redirect to ./url
func redirect(w http.ResponseWriter, url string) {
	_, _ = fmt.Fprint(w, "<head><meta http-equiv=\"Refresh\" content=\"0; URL=./"+url+"\"></head>")
}

// Handling of /logout
func doLogout(w http.ResponseWriter, r *http.Request) {
	logoutSession(w, r)
	redirect(w, "login")
}

// Handling of /index and redirecting to globalConfig.RedirectUrl
func showIndex(w http.ResponseWriter, r *http.Request) {
	err := templateFolder.ExecuteTemplate(w, "index", configuration.ServerSettings.RedirectUrl)
	helper.Check(err)
}

// Handling of /error
func showError(w http.ResponseWriter, r *http.Request) {
	err := templateFolder.ExecuteTemplate(w, "error", nil)
	helper.Check(err)
}

// Handling of /forgotpw
func forgotPassword(w http.ResponseWriter, r *http.Request) {
	err := templateFolder.ExecuteTemplate(w, "forgotpw", nil)
	helper.Check(err)
}

// Handling of /login
// Shows a login form. If username / pw combo is incorrect, client needs to wait for three seconds.
// If correct, a new session is created and the user is redirected to the admin menu
func showLogin(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	helper.Check(err)
	user := r.Form.Get("username")
	pw := r.Form.Get("password")
	failedLogin := false
	if pw != "" && user != "" {
		if strings.ToLower(user) == strings.ToLower(configuration.ServerSettings.AdminName) && configuration.HashPassword(pw, false) == configuration.ServerSettings.AdminPassword {
			createSession(w)
			redirect(w, "admin")
			return
		} else {
			time.Sleep(3 * time.Second)
			failedLogin = true
		}
	}
	err = templateFolder.ExecuteTemplate(w, "login", LoginView{
		IsFailedLogin: failedLogin,
		User:          user,
	})
	helper.Check(err)
}

// Variables for the login template
type LoginView struct {
	IsFailedLogin bool
	User          string
}

// Handling of /d
// Checks if a file exists for the submitted ID
// If it exists, a download form is shown or a password needs to be entered.
func showDownload(w http.ResponseWriter, r *http.Request) {
	keyId := queryUrl(w, r, "error")
	if keyId == "" {
		return
	}
	file := configuration.ServerSettings.Files[keyId]
	if file.ExpireAt < time.Now().Unix() || file.DownloadsRemaining < 1 {
		redirect(w, "error")
		return
	}
	if !helper.FileExists(configuration.Environment.DataDir + "/" + file.SHA256) {
		redirect(w, "error")
		return

	}
	view := DownloadView{
		Name:          file.Name,
		Size:          file.Size,
		Id:            file.Id,
		IsFailedLogin: false,
	}

	if file.PasswordHash != "" {
		r.ParseForm()
		enteredPassword := r.Form.Get("password")
		if configuration.HashPassword(enteredPassword, true) != file.PasswordHash && !isValidPwCookie(r, file) {
			if enteredPassword != "" {
				view.IsFailedLogin = true
				time.Sleep(1 * time.Second)
			}
			err := templateFolder.ExecuteTemplate(w, "download_password", view)
			helper.Check(err)
			return
		} else {
			if !isValidPwCookie(r, file) {
				writeFilePwCookie(w, file)
				// redirect so that there is no post data to be resent if user refreshes page
				redirect(w, "d?id="+file.Id)
				return
			}
		}
	}
	err := templateFolder.ExecuteTemplate(w, "download", view)
	helper.Check(err)
}

// Handling of /delete
// User needs to be admin. Deleted the requested file
func deleteFile(w http.ResponseWriter, r *http.Request) {
	if !isAuthenticated(w, r, false) {
		return
	}
	keyId := queryUrl(w, r, "admin")
	if keyId == "" {
		return
	}
	item := configuration.ServerSettings.Files[keyId]
	item.ExpireAt = 0
	configuration.ServerSettings.Files[keyId] = item
	storage.CleanUp(false)
	redirect(w, "admin")
}

// Checks if a file is associated with the GET parameter from the current URL
// Stops for 500ms to limit brute forcing if invalid key and redirects to redirectUrl
func queryUrl(w http.ResponseWriter, r *http.Request, redirectUrl string) string {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < configuration.ServerSettings.LengthId {
		time.Sleep(500 * time.Millisecond)
		redirect(w, redirectUrl)
		return ""
	}
	return keys[0]
}

// Handling of /admin
// If user is authenticated, this menu lists all uploads and enables uploading new files
func showAdminMenu(w http.ResponseWriter, r *http.Request) {
	if !isAuthenticated(w, r, false) {
		return
	}
	err := templateFolder.ExecuteTemplate(w, "admin", (&UploadView{}).convertGlobalConfig())
	helper.Check(err)
}

// Parameters for the download template
type DownloadView struct {
	Name          string
	Size          string
	Id            string
	IsFailedLogin bool
}

// Parameters for the admin menu template
type UploadView struct {
	Items            []filestructure.File
	Url              string
	TimeNow          int64
	DefaultDownloads int
	DefaultExpiry    int
	DefaultPassword  string
}

// Converts the globalConfig variable to an UploadView struct to pass the infos to
// the admin template
func (u *UploadView) convertGlobalConfig() *UploadView {
	var result []filestructure.File
	for _, element := range configuration.ServerSettings.Files {
		result = append(result, element)
	}
	sort.Slice(result[:], func(i, j int) bool {
		return result[i].ExpireAt > result[j].ExpireAt
	})
	u.Url = configuration.ServerSettings.ServerUrl + "d?id="
	u.DefaultPassword = configuration.ServerSettings.DefaultPassword
	u.Items = result
	u.DefaultExpiry = configuration.ServerSettings.DefaultExpiry
	u.DefaultDownloads = configuration.ServerSettings.DefaultDownloads
	u.TimeNow = time.Now().Unix()
	return u
}

// Handling of /upload
// If the user is authenticated, this parses the uploaded file from the Multipart Form and
// adds it to the system.
func uploadFile(w http.ResponseWriter, r *http.Request) {
	if !isAuthenticated(w, r, true) {
		return
	}
	err := r.ParseMultipartForm(20 * 1024 * 1024)
	responseError(w, err)
	allowedDownloads := r.Form.Get("allowedDownloads")
	expiryDays := r.Form.Get("expiryDays")
	password := r.Form.Get("password")
	allowedDownloadsInt, err := strconv.Atoi(allowedDownloads)
	if err != nil {
		allowedDownloadsInt = configuration.ServerSettings.DefaultDownloads
	}
	expiryDaysInt, err := strconv.Atoi(expiryDays)
	if err != nil {
		expiryDaysInt = configuration.ServerSettings.DefaultExpiry
	}
	configuration.ServerSettings.DefaultExpiry = expiryDaysInt
	configuration.ServerSettings.DefaultDownloads = allowedDownloadsInt
	configuration.ServerSettings.DefaultPassword = password
	file, handler, err := r.FormFile("file")
	responseError(w, err)
	result, err := storage.NewFile(&file, handler, time.Now().Add(time.Duration(expiryDaysInt)*time.Hour*24).Unix(), allowedDownloadsInt, password)
	responseError(w, err)
	defer file.Close()
	_, err = fmt.Fprint(w, result.ToJsonResult(configuration.ServerSettings.ServerUrl))
	helper.Check(err)
}

// Outputs an error in json format
func responseError(w http.ResponseWriter, err error) {
	if err != nil {
		fmt.Fprint(w, "{\"Result\":\"error\",\"ErrorMessage\":\""+err.Error()+"\"}")
		panic(err)
	}
}

// Outputs the file to the user and reduces the download remaining count for the file
func downloadFile(w http.ResponseWriter, r *http.Request) {
	keyId := queryUrl(w, r, "error")
	if keyId == "" {
		return
	}
	savedFile := configuration.ServerSettings.Files[keyId]
	if savedFile.DownloadsRemaining == 0 || savedFile.ExpireAt < time.Now().Unix() || !helper.FileExists(configuration.Environment.DataDir+"/"+savedFile.SHA256) {
		redirect(w, "error")
		return
	}
	if savedFile.PasswordHash != "" {
		if !(isValidPwCookie(r, savedFile)) {
			redirect(w, "d?id="+savedFile.Id)
			return
		}
	}
	savedFile.DownloadsRemaining = savedFile.DownloadsRemaining - 1
	configuration.ServerSettings.Files[keyId] = savedFile
	configuration.Save()

	w.Header().Set("Content-Disposition", "attachment; filename=\""+savedFile.Name+"\"")
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	file, err := os.OpenFile(configuration.Environment.DataDir+"/"+savedFile.SHA256, os.O_RDONLY, 0644)
	defer file.Close()
	helper.Check(err)
	_, err = io.Copy(w, file)
	helper.Check(err)
}

// Checks if the user is logged in as an admin
func isAuthenticated(w http.ResponseWriter, r *http.Request, isUpload bool) bool {
	if isValidSession(w, r) {
		return true
	}
	if isUpload {
		_, err := fmt.Fprint(w, "{\"Result\":\"error\",\"ErrorMessage\":\"Not authenticated\"}")
		helper.Check(err)
	} else {
		redirect(w, "login")
	}
	return false
}

// Write a cookie if the user has entered a correct password for a password-protected file
func writeFilePwCookie(w http.ResponseWriter, file filestructure.File) {
	http.SetCookie(w, &http.Cookie{
		Name:    "p" + file.Id,
		Value:   file.PasswordHash,
		Expires: time.Now().Add(5 * time.Minute),
	})
}

// Checks if a cookie contains the correct password hash for a password-protected file
// If incorrect, a 3 second delay is introduced unless the cookie was empty.
func isValidPwCookie(r *http.Request, file filestructure.File) bool {
	cookie, err := r.Cookie("p" + file.Id)
	if err == nil {
		if cookie.Value == file.PasswordHash {
			return true
		} else {
			time.Sleep(3 * time.Second)
		}
	}
	return false
}