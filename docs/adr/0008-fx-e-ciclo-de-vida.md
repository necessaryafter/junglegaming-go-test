# ADR 0008 — Composição e ciclo de vida com Fx

## Decisão

Usar Fx na composição, sem levá-lo ao domínio. `app` reúne módulos, logger e prazos
globais. Os adapters registram construtores e hooks em `module.go`. Rotas são
contribuídas pelo grupo `http_routes`; `platform/httpserver` controla o listener.

Verificar banco/schema, JWKS e filas antes de aceitar trabalho. Na parada, drenar
HTTP, cancelar workers e fechar o pool por último. Falha fatal do listener solicita
shutdown do Fx. Os endpoints operacionais ainda ficam no adapter HTTP financeiro.

## Motivo

Centralizar o ciclo de vida torna explícita a ordem de início e encerramento.
Fechar o pool por último permite que o trabalho em andamento termine ou seja
cancelado antes de perder acesso ao banco. Mensagens não confirmadas ficam
disponíveis para reentrega. A contribuição de rotas evita acoplar o servidor aos módulos.
