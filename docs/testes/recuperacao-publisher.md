# Recuperação do publisher da outbox

`TestPublisherRecoversAfterInterruption`, em
`internal/wagering/worker/publisher_recovery_integration_test.go`, exercita duas
janelas de queda usando o loop de publicação de produção em subprocessos reais.

| Janela | Interrupção | Recuperação esperada |
| --- | --- | --- |
| Depois do claim, antes do envio | O proxy bloqueia SendMessage sem encaminhá-lo ao SQS; o processo é encerrado à força. | Outro processo assume após expirar o lease, envia o evento e confirma a outbox. |
| Depois do aceite pelo SQS, antes da confirmação SQL | O proxy encaminha SendMessage, observa a resposta de sucesso do SQS e a retém; o processo é encerrado à força. | Outro processo assume após expirar o lease, repete o envio com a mesma identidade e confirma a outbox. |

Cada cenário cria schema e filas FIFO exclusivos, aplica migrations, abre uma
carteira com `100.00 BRL` e processa uma BET de `25.00 BRL`. Seu evento
WalletBalanceChanged é o alvo da interrupção. Os outros eventos genuínos ficam
pendentes, com o próximo envio adiado, para isolar a disputa pelo mesmo registro.

O processo principal confere que o evento foi reivindicado e ainda não está
publicado antes de encerrar o publisher. O substituto tem memória e conexões
próprias; a expiração do lease ocorre no PostgreSQL, sem alterar lease_until
manualmente. Ao final, o teste verifica:

- Duas reivindicações, nenhuma falha de publicação confirmada e lease liberado.
- Publicação concluída após o prazo do lease abandonado.
- Mesmo eventId, payload exato, MessageGroupId da carteira e MessageDeduplicationId.
- Mensagem real recebida da fila de saída com o snapshot persistido.
- Carteira, transações, ledger, inbox e dados imutáveis dos eventos preservados.
- Reconciliação consistente com abertura e um único débito.

Na segunda janela, as duas chamadas aceitas são observadas no proxy. A fila FIFO
pode suprimir a segunda entrega dentro da janela de deduplicação; o teste não usa
uma segunda mensagem visível como prova da republicação. A identidade estável
continua necessária para consumidores fora dessa janela.

O failpoint existe apenas no proxy de teste. A resposta de sucesso fica retida
antes de chegar ao SDK, simulando também a incerteza de um aceite cuja confirmação
se perdeu. O subprocesso de teste usa lease de três segundos; a configuração de
produção mantém o padrão de 60 segundos e mínimo de 20. O teste não inicia o
consumidor de entrada nem o worker de referências. O teste do
[consumidor](recuperacao-consumidor.md) continua verificando sua própria janela.

O broker local é o MiniStack com `AUTH=true`, conforme o Compose. A fixture
compartilhada exige as três variáveis abaixo e cria material de assinatura
Ed25519 isolado; este cenário submete a BET diretamente por `postgres.Store.Submit` e
publica o snapshot da outbox, sem JWT nem `MessageSignature` nos eventos de saída.
A autenticação durável de mensagens de entrada é exercitada pelo
[teste do consumidor](recuperacao-consumidor.md); as políticas IAM por produtor
são verificadas separadamente em `TestRealProducerPermissions`.

Prepare as dependências conforme o [README](../../README.md#testes) e execute:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./internal/wagering/worker \
  -run '^TestPublisherRecoversAfterInterruption$' -count=1 -v
```

O auxiliar TestPublisherRecoveryProcess é invocado pelo teste pai; seu skip na
execução normal é esperado. Fixtures e controle dos subprocessos são compartilhados
com o teste de recuperação do consumidor, com limpeza de filas, schemas e processos.
A [matriz com três aplicações](recuperacao-tres-processos.md) cobre essas quedas
com publishers concorrentes. O [redrive/DLQ](redrive-dlq.md) e a
[recuperação após falha de banco](recuperacao-banco-dlq.md) também têm cobertura
automatizada.
