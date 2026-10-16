# Recuperação com três aplicações independentes

Os testes `TestThreeProcesses*` em `internal/app` iniciam três subprocessos do
executável de testes, cada um executando a composição Fx completa, com seus próprios
workers, conexões PostgreSQL e memória. A fixture confere PIDs distintos, health
checks e três application_name diferentes em pg_stat_activity.

Cada teste tem schema e filas FIFO exclusivos. O listener de teste expõe o handler
HTTP de produção, e a composição também inicia e encerra o listener normal de
platform/httpserver em porta efêmera. Nenhum teste usa portas fixas de aplicação.

| Cenário | Evidência |
| --- | --- |
| 50 duplicatas | Uma transação processada, 49 replays e um único débito, distribuídos entre três aplicações. |
| Disputa de saldo | Duas BETs de 80.00 sobre 100.00: uma processada, outra rejeitada e saldo final de 20.00. |
| Carteiras independentes | O teste segura o lock SQL de uma carteira e comprova que outra avança por outra aplicação antes da liberação. |
| Restart financeiro | Substituição forçada de todo o grupo preserva resultados de operações processadas e rejeitadas. |
| Consumidor após commit | Um proxy retém o delete do primeiro consumidor, que é encerrado. Os sobreviventes recebem a mesma mensagem após expirar a visibilidade real de 60 segundos. HTTP e SQS reconhecem o resultado persistido. |
| Publisher antes do envio | Um processo é encerrado após o claim. Outro publisher já em execução retoma após expirar o lease. |
| Publisher após aceite | O proxy retém a confirmação de SendMessage após sucesso no SQS. Outro publisher repete o mesmo evento e confirma a outbox. |
| Referência pendente | REFUND e ROLLBACK sobrevivem à substituição das três aplicações e são resolvidos quando a BET chega. Outro restart preserva o replay. |
| Referência expirada | Fixtures históricas são liberadas para retry apenas depois de substituir o grupo. A rejeição permanece após chegada tardia da BET e novo restart, com saldo original no replay. |
| Falha de banco e DLQ | Escritas abortadas após o INSERT do ledger não deixam efeitos parciais. Após o redrive e a remoção da falha, todo o grupo é substituído e processa a mensagem recuperada. |
| Entrada rejeitada e DLQ | Campo desconhecido, ausência de credencial, assinatura inválida e identidade de outro provedor atravessam o restart, preservando o contador de recebimentos do broker e chegando à DLQ sem efeitos financeiros. |

HTTP usa tokens do Keycloak real. Para entradas SQS,
`internal/testsupport/sqsidentity` gera chaves Ed25519 distintas por provedor e
um registro de chaves públicas isolado, herdado pelos três processos e seus
substitutos. O atributo `MessageSignature` contém JWS destacado EdDSA sobre os
bytes exatos do corpo; não há token com expiração na fila. Na matriz de rejeições,
uma assinatura de `provider-b` sobre corpo que declara `provider-a` é válida
criptograficamente e rejeitada pela autorização do caso de uso. A matriz também
confere preservação do atributo no redrive. A segregação das credenciais e
políticas IAM do broker é demonstrada separadamente por
`TestRealProducerPermissions`; as fixtures de recuperação usam acesso
administrativo local para criar e limpar seus recursos.

Proxies de teste controlam somente as janelas de queda e observam chamadas reais;
a lógica financeira, a autenticação e os loops dos workers são os de produção.
O lease da outbox usa a configuração válida de 20 segundos. Os testes SQS mantêm
a visibilidade de 60 segundos e o backoff de produção, sem alterar o relógio ou
transferir manualmente mensagens para a DLQ.
Recebimentos abandonados durante a queda podem incrementar o contador do broker
sem que sua resposta chegue ao processo ou ao observador. A matriz exige que esse
contador seja preservado e permaneça dentro do limite, admitindo lacunas. O
reprocessamento recuperável também admite reentregas e exige um único efeito
financeiro, preservação da identidade e replay durável.

Os testes de expiração inserem pendências históricas no schema de teste, sem
editar timestamps imutáveis. A atualização de next_attempt_at fica sem commit
até os três processos serem substituídos. Assim, somente o novo grupo pode tratar
a expiração. Os testes normais de referência pendente entram pela API HTTP.

Snapshots incluem carteira, transações, ledger, inbox e dados imutáveis dos eventos;
flags de publicação e lease ficam fora da comparação porque publishers continuam
ativos. Reconciliação é consultada pelas três APIs. No teste FIFO, duas chamadas
SendMessage aceitas com o mesmo eventId podem produzir uma única entrega visível
pela deduplicação do broker.

Prepare PostgreSQL, IdP e MiniStack com `AUTH=true` conforme o [README](../../README.md#testes):

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./internal/app \
  -run '^TestThreeProcesses' -count=1 -v
```

A execução leva vários minutos por respeitar os prazos reais. TestClusterProcess
é invocado pela fixture; seu skip na execução normal é esperado. Subprocessos,
schemas e filas temporários são removidos ao final. A matriz simula interrupções
de processos e erros SQL determinísticos; perda de rede e resultado incerto de
COMMIT exigem cenários próprios, fora desta cobertura.
