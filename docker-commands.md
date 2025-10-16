docker compose up --build -d
docker compose up -d 
docker compose down 
docker compose logs -f pub/sub
./traffic.sh --wifi-lat 5ms --wifi-loss 0% --lte-lat 10ms --lte-loss 100%
