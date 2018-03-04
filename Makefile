build:
	go build -o cydev-web
build-prod:
	/usr/local/go/bin/go build -o cydev-web
deploy:
	ansible-playbook -i hosts deploy.yml