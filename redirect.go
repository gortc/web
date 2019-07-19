package main

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

var tmpl = template.Must(template.New("main").Parse(`<!DOCTYPE html>
<html>
<head>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8"/>
<meta name="go-import" content="{{.ImportRoot}} {{.VCS}} {{.VCSRoot}}">
<meta name="go-source" content="{{.ImportRoot}} {{.VCSRoot}} {{.VCSRoot}}/tree/master{/dir} {{.VCSRoot}}/blob/master{/dir}/{file}#L{line}">
<meta http-equiv="refresh" content="0; url=https://godoc.org/{{.ImportRoot}}{{.Suffix}}">
</head>
<body>
Redirecting to docs at <a href="https://godoc.org/{{.ImportRoot}}{{.Suffix}}">godoc.org/{{.ImportRoot}}{{.Suffix}}</a>...
</body>
</html>
`))

type data struct {
	ImportRoot string
	VCS        string
	VCSRoot    string
	Suffix     string
}

func redirect(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimSuffix(req.Host+req.URL.Path, "/")
	var importRoot, repoRoot, suffix string
	if path == importPath {
		http.Redirect(w, req, "https://godoc.org/"+importPath, 302)
		return
	}
	fmt.Println(path, importPath)
	if !strings.HasPrefix(path, importPath+"/") {
		http.NotFound(w, req)
		return
	}
	elem := path[len(importPath)+1:]
	if i := strings.Index(elem, "/"); i >= 0 {
		elem, suffix = elem[:i], elem[i:]
	}
	importRoot = importPath + "/" + elem
	repoRoot = repoPath + "/" + elem
	d := &data{
		ImportRoot: importRoot,
		VCS:        "git",
		VCSRoot:    repoRoot,
		Suffix:     suffix,
	}
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, d)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(buf.Bytes())
}
