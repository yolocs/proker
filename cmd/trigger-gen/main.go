package main

import (
	"bytes"
	"flag"
	"io/ioutil"
	"log"
	"text/template"
)

var (
	total        = flag.Int("total", 100, "Number of triggers.")
	templateFile = flag.String("template", "template.yaml", "The trigger template.")
)

type triggerTemplate struct {
	Index int
}

func main() {
	flag.Parse()
	b, err := ioutil.ReadFile(*templateFile)
	if err != nil {
		log.Panicln("failed to read template:", err)
	}

	t, err := template.New("trigger").Parse(string(b))
	if err != nil {
		log.Panicln("failed to parse trigger template:", err)
	}

	out := bytes.NewBuffer([]byte{})
	for i := 0; i < *total; i++ {
		if err := t.Execute(out, &triggerTemplate{Index: i}); err != nil {
			log.Panicln("failed to execute template:", err)
		}
	}

	ioutil.WriteFile("triggers.yaml", out.Bytes(), 0644)
}
