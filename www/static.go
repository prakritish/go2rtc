package www

import "embed"

//go:embed *.html
//go:embed *.js
//go:embed *.json
//go:embed swagger
var Static embed.FS
