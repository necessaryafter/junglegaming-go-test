# ADR 0006 — Identidade verificada e isolamento por provedor

## Decisão

Usar Keycloak com `client_credentials` e go-oidc no HTTP para validar assinatura
RS256, issuer, audience e expiração. Verificar também `nbf`, subject e roles.
A role `provider` habilita a identidade `providerId`; `internal` permite operações
de carteira. Autorizar no caso de uso antes do repositório e filtrar consultas
externas pelo provedor autenticado. `identity.Principal` não carrega JWT ou HTTP.

Para SQS, combinar políticas IAM segregadas com assinatura durável do produtor.
O atributo `MessageSignature` contém uma JWS compacta detached, com algoritmo
`EdDSA` e chave Ed25519. Ela cobre os bytes exatos do corpo, incluindo espaços,
e usa os cabeçalhos protegidos `typ=jungle-sqs-v1` e `kid`. O consumidor aceita
somente esse formato e algoritmo. A assinatura não é um access token, não tem
expiração e não cria um emissor próprio de tokens de acesso. A integração OIDC
continua obrigatória para HTTP.

`SQS_PRODUCER_KEYS_FILE` aponta para um registro local de chaves públicas JWK,
carregado e validado no startup. Cada `kid` único vincula uma chave Ed25519 a um
`providerId`. Chaves privadas, chaves compartilhadas entre entradas e IDs de
chave duplicados são recusados; arquivo ausente ou inválido impede o startup.
O consumidor não busca chaves fornecidas por headers da mensagem. A identidade
obtida da chave passa pelo mesmo caso de uso do HTTP, que exige correspondência
com `data.providerId`. Ela nunca concede o papel interno de administração de
carteiras. Inbox e idempotência continuam obrigatórias; a assinatura fica fora
do corpo e preserva os hashes existentes.

O ambiente local usa MiniStack 1.5.27 com `AUTH=true`, estado persistente e
credenciais IAM distintas para `provider-a`, `provider-b` e `jungle-runtime`.
O nome Compose `localstack` é preservado por compatibilidade. O provisionamento
idempotente gera credenciais e chaves separadas em volumes privados por produtor
mais um volume de runtime com credencial própria e somente chaves públicas.
Ele reaplica as políticas versionadas e recusa grants adicionais inesperados
nos usuários que administra.

| Papel | Permissões |
| --- | --- |
| Produtor A/B | `GetQueueUrl` e `SendMessage` na fila de pedidos |
| Runtime | Consulta de atributos/URL das quatro filas; `ReceiveMessage`, `DeleteMessage` e `ChangeMessageVisibility` nos pedidos; `SendMessage` nos eventos; leitura CloudWatch |
| Provisionamento | Administração das filas e identidades locais |

As políticas SQS referenciam os ARNs específicos; `cloudwatch:GetMetricStatistics`
usa `Resource: "*"`. O runtime não publica pedidos nem monta chaves privadas
`Ed25519` dos produtores. Produtores não consomem pedidos, publicam eventos ou
administram filas. Políticas e material local estão em
[`docker/sqs`](../../docker/sqs); a aplicação acessa o broker com credencial
própria, sem credenciais administrativas como fallback.

## Motivo

O `providerId` enviado no corpo não comprova quem está chamando. Um JWT de curta
duração pode expirar durante retenção, indisponibilidade ou redrive. Uma assinatura
sobre o corpo conserva a identidade verificável durante essas etapas, enquanto
a chave pública correspondente permanecer confiada. IAM controla o envio e as
operações do broker; o registro de chaves permite ao consumidor verificar a
origem e o conteúdo após a entrega. Autorizar no caso de uso mantém a mesma
proteção em HTTP e SQS e impede acesso a transações de terceiros.

A edição Community do LocalStack usada anteriormente não fornecia a avaliação
IAM necessária para demonstrar o isolamento nesse ambiente. MiniStack é uma das
alternativas aceitas no desafio e permite exercitar as políticas e a rejeição de
credenciais incorretas localmente. Não se usa o `SenderId` do emulador como
identidade por produtor; a associação depende da assinatura verificada.

## Operação e limites

Credenciais especiais de administrador do MiniStack, como `test`/`test`,
contornam a avaliação IAM. São usadas pelo provisionamento e pelas fixtures
administrativas, nunca pelo runtime ou produtores. As portas do ambiente local
ficam restritas a loopback. O emulador é destinado a desenvolvimento; ambientes
AWS devem usar identidades, segredos e políticas equivalentes sob IAM nativo.

O bootstrap preserva as chaves locais `provider-a-v1` e `provider-b-v1`, mas não
automatiza rotação. A troca deve publicar uma nova chave pública com `kid` único,
manter a antiga e reiniciar todos os consumidores antes de mudar a chave privada
do produtor. Não há recarga automática do registro. Retenha as chaves antigas
por toda a janela de fila, DLQ, arquivos e redrives; a retenção local de cada fila
é de 14 dias, e a janela operacional pode ser maior. Remova-as apenas depois de
resolver as mensagens dependentes. Adapte o bootstrap para preservar o registro
rotacionado, pois sua próxima execução recompõe as duas entradas `-v1`.

Revogar IAM impede novos envios daquele usuário; assinaturas de mensagens já
aceitas continuam válidas. Remover a chave pública rejeita também suas mensagens
pendentes e exige investigação e eventual reprocessamento autorizado. O corpo
original, `messageId` e chave de idempotência devem ser preservados ao re-assinar.
O bootstrap reaplica as políticas `Allow` e substitui credenciais IAM inativas;
revogações permanentes exigem adaptar ou desabilitar esse reconciliador antes
de executá-lo novamente, para não reautorizar o produtor. Os procedimentos e
comandos estão no [README](../../README.md#sqs).

Mensagens antigas com apenas `AccessToken` não têm fallback no novo consumidor.
Drene-as com a versão anterior antes do rollout, enquanto os tokens forem
válidos, ou investigue e obtenha nova assinatura do produtor autorizado.
A substituição do broker não migra automaticamente o estado anterior.

## Verificação

- [Testes de assinatura](../../internal/platform/messageauth/signature_test.go):
  identidade durável com corpo antigo, reload do registro, bytes adulterados,
  chave desconhecida, algoritmos/tipos indevidos e registro inválido.
- [Permissões reais](../../internal/platform/queue/security_integration_test.go):
  identidades IAM separadas, operações permitidas/negadas conforme as políticas
  versionadas, secret incorreta recusada e revogação de A sem bloquear B.
- [Matriz de redrive](../../internal/wagering/worker/dlq_integration_test.go):
  assinatura ausente, adulterada e assinatura legítima de B sobre operação de A.
  Cada mensagem passa pelos cinco recebimentos e pelo backoff de produção,
  chega à DLQ preservando corpo e atributos, sem conclusão da inbox, efeitos
  financeiros ou credenciais nos logs.
- [Autorização HTTP](../../internal/app/app_integration_test.go): isolamento
  entre provedores nas consultas e token real do Keycloak aceito antes da
  expiração e rejeitado depois, sem persistir a nova operação.
