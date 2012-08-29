all:

app_engine = ~/opt/google_appengine

deploy:
	$(app_engine)/appcfg.py update usvisa/

local:
	$(app_engine)/dev_appserver.py usvisa/

fmt_flags = -w=true -tabs=false -tabwidth=2

format:
	$(MAKE) formatter name=./usvisa/usvisa/usvisa

formatter:
	gofmt $(fmt_flags) $(name).go