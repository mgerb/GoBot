install:
	cd server && dep ensure && go get
	cd client && npm install

build-server:
	cd ./server && packr build -o ../bot ./main.go

build-scripts:
	go build -o bot-scripts ./server/scripts

build-client:
	cd client && npm run build

clean:
	rm -rf bot ./dist

all: install build-client build-server
