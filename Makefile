build:
	go build -o cydev-web
build-prod:
	@/usr/local/go/bin/go build -v -o cydev-web
deploy:
	cd provision && ansible-playbook -i hosts deploy.yml
