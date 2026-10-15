# ADR 0008 — Composição e ciclo de vida com Fx

## Decisão

Usar Fx na composição, sem levá-lo ao domínio. `app` reúne módulos, logger e prazos
globais. Os adapters registram construtores e hooks em `module.go`. Rotas são
contribuídas pelo grupo `http_routes`; `platform/httpserver` controla o listener.

Verificar banco/schema, JWKS e filas antes de aceitar trabalho. Na parada, drenar
HTTP, cancelar workers e fechar o pool por último. Falha fatal do listener solicita
shutdown do Fx. Os endpoints operacionais ainda ficam no adapter HTTP financeiro.

No package `wagering/worker`, consumidor SQS, publisher da outbox, retomada de
referências e coletor de backlog são componentes distintos, com construtores
próprios. O consumidor recebe fila, caso de uso, verificador, logger e métricas;
o publisher recebe store, fila, logger, métricas e os prazos necessários;
referências recebem apenas o store; o coletor recebe store e métricas.

`module.go` compõe esses componentes pelo Fx. `lifecycle.go` inicia suas tarefas
com um contexto independente do prazo de startup, cancela todas na parada e
aguarda o término dentro do prazo de shutdown. Essa coordenação não recebe
repositórios, autenticação ou casos de uso. Os testes permanecem junto dos
componentes no mesmo package.

## Motivo

Construtores por tarefa explicitam suas dependências e permitem exercitar cada
componente separadamente. Centralizar o ciclo de vida torna explícita a ordem
de início e encerramento.
Fechar o pool por último permite que o trabalho em andamento termine ou seja
cancelado antes de perder acesso ao banco. Mensagens não confirmadas ficam
disponíveis para reentrega. A contribuição de rotas evita acoplar o servidor aos módulos.
